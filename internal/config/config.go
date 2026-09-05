// Package config reads and writes the go-fs configuration file.
//
// The file is TOML. Every key is optional: a value that is absent keeps the
// built-in default, which mirrors the defaults of the original Node
// implementation. Load therefore unmarshals onto a fully populated defaults
// struct rather than onto a zero value.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/crypto/ssh"
)

//go:embed template.toml
var template []byte

// Template returns the documented starter configuration.
func Template() []byte {
	return template
}

// Config is the whole configuration file.
type Config struct {
	General General `toml:"general"`
	Log     Log     `toml:"log"`
	FTP     FTP     `toml:"ftp"`
	FTPS    FTPS    `toml:"ftps"`
	SFTP    SFTP    `toml:"sftp"`
	HTTP    HTTP    `toml:"http"`
	HTTPS   HTTPS   `toml:"https"`
	TFTP    TFTP    `toml:"tftp"`
}

// General holds what every server shares.
type General struct {
	// Basefolder is the folder the servers fall back to when their own
	// section does not name one, so that a configuration where they all serve
	// the same tree says it once. It has to be an absolute path.
	Basefolder string `toml:"basefolder"`

	// ReloadConfig watches the configuration file and applies what changes in
	// it without a restart. A file that does not parse or does not validate is
	// reported and ignored, so a half-written save cannot take a server down.
	ReloadConfig bool `toml:"reloadConfig"`
	// ReloadInterval is how many seconds pass between two checks of the file.
	ReloadInterval int `toml:"reloadInterval"`

	// AdminInterfaceEnabled serves the web interface that edits this file. It
	// is off unless it is switched on, so that an upgrade never opens it by
	// itself.
	AdminInterfaceEnabled bool `toml:"adminInterfaceEnabled"`
	// AdminInterfaceAddress is the interface it binds to. It is the loopback
	// address by default, because the page shows and edits every password in
	// this file; set it to an empty string to bind every interface.
	AdminInterfaceAddress string `toml:"adminInterfaceAddress"`
	AdminInterfacePort    int    `toml:"adminInterfacePort"`
	// AdminUsername and AdminPassword are the single account of the web
	// interface. Both have to be set for it to start.
	AdminUsername string `toml:"adminUsername"`
	AdminPassword string `toml:"adminPassword"`
	// AdminCert and AdminKey are PEM file paths. The interface is always
	// served over TLS; when both are empty a self-signed certificate is
	// generated at startup.
	AdminCert string `toml:"adminCert"`
	AdminKey  string `toml:"adminKey"`
}

// Log controls the diagnostics the servers produce. The original emitted
// 'log', 'debug', 'warn', 'error', 'listen', 'login', 'logoff', 'download' and
// 'upload' events; here they become structured log records.
type Log struct {
	// Level is one of debug, info, warn, error. The protocol trace that the
	// original reported as 'log' events is written at debug level.
	Level string `toml:"level"`
	// Format is text or json.
	Format string `toml:"format"`
}

// FTPS configures the TLS interface of the FTP server. It is a section of its
// own because TOML tables are top level; the folders, accounts and limits of
// [ftp] apply to this listener too.
type FTPS struct {
	// Enabled makes the server listen on Port and offer AUTH TLS on the plain
	// port. It is independent of FTP.Enabled: with that one off the server
	// serves implicit FTPS only.
	Enabled bool `toml:"enabled"`
	Port    int  `toml:"port"`
	// Cert and Key are PEM file paths. When both are empty a self-signed
	// certificate is generated at startup.
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

// User is one entry of the FTP user list. There is nothing special about the
// account named "anonymous": it is an ordinary entry that sets
// allowLoginWithoutPassword.
//
// The permission flags are pointers only so that Save can leave an unset key
// out of the file; every one of them denies by default.
type User struct {
	Username                  string `toml:"username"`
	Password                  string `toml:"password"`
	Basefolder                string `toml:"basefolder,omitempty"`
	AllowLoginWithoutPassword *bool  `toml:"allowLoginWithoutPassword,omitempty"`
	AllowUserFileCreate       *bool  `toml:"allowUserFileCreate,omitempty"`
	AllowUserFileRetrieve     *bool  `toml:"allowUserFileRetrieve,omitempty"`
	AllowUserFileOverwrite    *bool  `toml:"allowUserFileOverwrite,omitempty"`
	AllowUserFileDelete       *bool  `toml:"allowUserFileDelete,omitempty"`
	AllowUserFolderDelete     *bool  `toml:"allowUserFolderDelete,omitempty"`
	AllowUserFolderCreate     *bool  `toml:"allowUserFolderCreate,omitempty"`

	// AuthorizedKeys are SSH public keys in authorized_keys format, one entry
	// per line as ssh-keygen writes them. They are how an SFTP account logs in
	// with a key instead of a password; the FTP server ignores them.
	AuthorizedKeys []string `toml:"authorizedKeys,omitempty"`
}

// Permissions resolves the user entry. Every right has to be granted
// explicitly: an account that sets none of the flags can log in and look
// around, and nothing else.
func (u User) Permissions() Permissions {
	return Permissions{
		Basefolder:      u.Basefolder,
		LoginNoPassword: boolOr(u.AllowLoginWithoutPassword, false),
		FileCreate:      boolOr(u.AllowUserFileCreate, false),
		FileRetrieve:    boolOr(u.AllowUserFileRetrieve, false),
		FileOverwrite:   boolOr(u.AllowUserFileOverwrite, false),
		FileDelete:      boolOr(u.AllowUserFileDelete, false),
		FolderDelete:    boolOr(u.AllowUserFolderDelete, false),
		FolderCreate:    boolOr(u.AllowUserFolderCreate, false),
	}
}

// Permissions is what a logged in session is allowed to do.
type Permissions struct {
	Basefolder      string
	LoginNoPassword bool
	FileCreate      bool
	FileRetrieve    bool
	FileOverwrite   bool
	FileDelete      bool
	FolderDelete    bool
	FolderCreate    bool
}

// FTP configures the FTP server.
type FTP struct {
	// Enabled serves the plain control port. The TLS listener has its own
	// switch in [ftps]; the rest of this section applies to both.
	Enabled    bool   `toml:"enabled"`
	Port       int    `toml:"port"`
	Basefolder string `toml:"basefolder"`

	MaxConnections   int `toml:"maxConnections"`
	MinDataPort      int `toml:"minDataPort"`
	IdleTimeout      int `toml:"idleTimeout"`
	DataTimeout      int `toml:"dataTimeout"`
	MaxCommandLength int `toml:"maxCommandLength"`
	// LoginFailureDelay is the delay in seconds before a wrong password is
	// answered, which slows down guessing.
	LoginFailureDelay int `toml:"loginFailureDelay"`

	// AllowFtpBounce permits PORT and EPRT to name a host other than the
	// client (RFC 2577).
	AllowFtpBounce bool `toml:"allowFtpBounce"`
	// AllowForeignDataConnection permits passive data connections from a
	// different address than the control connection.
	AllowForeignDataConnection bool `toml:"allowForeignDataConnection"`

	// Users are the accounts, including anonymous access. There is no default
	// account and no implicit one: a name that is not listed cannot log in.
	Users []User `toml:"users"`
}

// SFTP configures the SFTP server, which is the SFTP subsystem of an SSH
// server. It has the shape of the FTP section: a base folder and a list of
// accounts, with the same permission flags.
type SFTP struct {
	Enabled    bool   `toml:"enabled"`
	Port       int    `toml:"port"`
	Basefolder string `toml:"basefolder"`

	// HostKey is the SSH host key itself rather than a path to it, base64 of
	// its PEM encoding, so that the whole configuration stays in one file.
	// When it is empty a key is generated at startup, which every client will
	// report as a changed host key after a restart.
	HostKey string `toml:"hostkey"`

	MaxConnections int `toml:"maxConnections"`
	// IdleTimeout is the number of seconds without any traffic after which a
	// connection is closed, 0 disables it.
	IdleTimeout int `toml:"idleTimeout"`
	// LoginFailureDelay is the delay in seconds before a wrong password is
	// answered, which slows down guessing.
	LoginFailureDelay int `toml:"loginFailureDelay"`

	// Users are the accounts. Each needs a password or at least one authorized
	// key; allowLoginWithoutPassword has no meaning here, because SSH has no
	// equivalent of an anonymous login.
	Users []User `toml:"users"`
}

// HTTP configures the HTTP file server: browsing and downloading with GET,
// uploading with PUT and removing with DELETE.
//
// Access has two layers. A request is public unless its method is in
// MethodsRequireAuth or its path matches one of PathsRequireAuth; when it is
// not public it has to be answered by one of Users, and that account's own
// paths and rights then decide what it may do.
type HTTP struct {
	// Enabled serves the plain port. The TLS listener has its own switch in
	// [https]; the rest of this section applies to both.
	Enabled    bool   `toml:"enabled"`
	Port       int    `toml:"port"`
	Basefolder string `toml:"basefolder"`

	// Realm is what clients are challenged with and, because it is hashed into
	// the digest response, changing it invalidates saved credentials.
	Realm string `toml:"realm"`

	MaxConnections int `toml:"maxConnections"`
	// ReadTimeout, WriteTimeout and IdleTimeout are seconds, 0 disables one.
	// WriteTimeout is off by default: it would cap the duration of a download.
	ReadTimeout  int `toml:"readTimeout"`
	WriteTimeout int `toml:"writeTimeout"`
	IdleTimeout  int `toml:"idleTimeout"`
	// MaxUploadSize is the largest accepted body in bytes, 0 means no limit.
	MaxUploadSize int64 `toml:"maxUploadSize"`
	// SessionTimeout is how long a session cookie stays valid, in seconds.
	SessionTimeout int `toml:"sessionTimeout"`
	// LoginFailureDelay is the delay in seconds before a rejected request is
	// answered, which slows down guessing.
	LoginFailureDelay int `toml:"loginFailureDelay"`

	// MethodsRequireAuth are the methods that always need an account.
	MethodsRequireAuth []string `toml:"methodsRequireAuth"`
	// PathsRequireAuth are regular expressions; a request whose path matches
	// one of them needs an account whatever its method.
	PathsRequireAuth []string `toml:"pathsRequireAuth"`

	Cleanup []Cleanup  `toml:"cleanup"`
	Users   []HTTPUser `toml:"users"`
}

// HTTPS configures the TLS interface of the HTTP server. It is a section of
// its own because TOML tables are top level; the folder, accounts and limits
// of [http] apply to this listener too.
type HTTPS struct {
	// Enabled makes the server listen on Port. It is independent of
	// HTTP.Enabled: with that one off the server serves HTTPS only.
	Enabled bool `toml:"enabled"`
	Port    int  `toml:"port"`
	// Cert and Key are PEM file paths. When both are empty a self-signed
	// certificate is generated at startup.
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

// Cleanup keeps a folder from growing without bound: everything but the Keep
// newest files in it is deleted, once an hour.
type Cleanup struct {
	Path string `toml:"path"`
	Keep int    `toml:"keep"`
}

// HTTPUser is one entry of http.users. It is not the User of the other servers:
// an HTTP account is scoped by path patterns rather than by a base folder, and
// the operations it can be granted are different ones.
type HTTPUser struct {
	Username string `toml:"username"`
	Password string `toml:"password"`

	// Paths are regular expressions matched against the request path, after it
	// has been normalized, so that ".." cannot be used to slip past one. An
	// account with no pattern can reach nothing.
	Paths []string `toml:"paths"`

	// Both default to false, as the permissions of the other servers do.
	AllowUserFileUpload bool `toml:"allowUserFileUpload"`
	AllowUserFileDelete bool `toml:"allowUserFileDelete"`

	// Cookie hands the client a session cookie once it has authenticated, so
	// that a browser does not repeat the credentials on every request. The
	// session is bound to this account and carries exactly these rights.
	Cookie bool `toml:"cookie"`
	// CookiePath is the URL prefix the browser sends the cookie back for, "/"
	// when it is not set. It is a hint to the browser, not a permission: the
	// paths above are checked on every request either way.
	CookiePath string `toml:"cookiePath,omitempty"`
}

// TFTP configures the TFTP server.
type TFTP struct {
	Enabled    bool   `toml:"enabled"`
	Port       int    `toml:"port"`
	Address    string `toml:"address"`
	Type       string `toml:"type"`
	Basefolder string `toml:"basefolder"`

	AllowRead            bool `toml:"allowRead"`
	AllowWrite           bool `toml:"allowWrite"`
	AllowOverwrite       bool `toml:"allowOverwrite"`
	AllowCreateDirectory bool `toml:"allowCreateDirectory"`

	MaxConnections        int   `toml:"maxConnections"`
	MaxConnectionsPerHost int   `toml:"maxConnectionsPerHost"`
	Timeout               int   `toml:"timeout"`
	MaxTimeout            int   `toml:"maxTimeout"`
	Retries               int   `toml:"retries"`
	TransferTimeout       int   `toml:"transferTimeout"`
	MaxBlockSize          int   `toml:"maxBlockSize"`
	MaxWindowSize         int   `toml:"maxWindowSize"`
	MaxFileSize           int64 `toml:"maxFileSize"`
}

// Default returns the configuration that applies when nothing is set. The
// values match the FTPdefaults, UserDefaults and TFTPdefaults of the original.
func Default() Config {
	return Config{
		General: General{
			ReloadConfig:          true,
			ReloadInterval:        5,
			AdminInterfaceAddress: "127.0.0.1",
			AdminInterfacePort:    10443,
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
		FTP: FTP{
			Enabled:           true,
			Port:              21,
			MaxConnections:    10,
			MinDataPort:       1024,
			IdleTimeout:       600,
			DataTimeout:       5,
			MaxCommandLength:  4096,
			LoginFailureDelay: 1,
		},
		FTPS: FTPS{
			Port: 990,
		},
		// SFTP is the one server that is off by default: a configuration
		// written for an earlier version has no [sftp] section, and starting
		// an SSH listener on such an upgrade would be a surprise.
		SFTP: SFTP{
			Port:              22,
			MaxConnections:    10,
			IdleTimeout:       600,
			LoginFailureDelay: 1,
		},
		HTTP: HTTP{
			Port:               9080,
			Realm:              "go-fs",
			MaxConnections:     100,
			ReadTimeout:        120,
			IdleTimeout:        120,
			SessionTimeout:     86400,
			LoginFailureDelay:  1,
			MethodsRequireAuth: []string{"PUT", "DELETE", "POST"},
		},
		HTTPS: HTTPS{
			Port: 9443,
		},
		TFTP: TFTP{
			Enabled:               true,
			Port:                  69,
			AllowRead:             true,
			AllowCreateDirectory:  true,
			MaxConnections:        10,
			MaxConnectionsPerHost: 5,
			Timeout:               5,
			MaxTimeout:            60,
			Retries:               3,
			TransferTimeout:       300,
			MaxBlockSize:          1468,
			MaxWindowSize:         16,
		},
	}
}

// Parse unmarshals data onto the defaults and stops there. It is what an editor
// of this file wants: the sections say what the file says, so that writing the
// result back does not turn an inherited value into an explicit one.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Load reads path onto the defaults and validates the result.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Default(), err
	}
	cfg, err := Parse(data)
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	cfg = cfg.Resolved()
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Resolved is a copy that hands general.basefolder to every server that did not
// name one of its own. It is applied before validation, so an error names the
// section the folder ended up in rather than the one it came from.
//
// The copy is shallow, which is safe because only string fields are written.
func (c Config) Resolved() Config {
	if c.General.Basefolder == "" {
		return c
	}
	for _, folder := range []*string{
		&c.FTP.Basefolder,
		&c.SFTP.Basefolder,
		&c.HTTP.Basefolder,
		&c.TFTP.Basefolder,
	} {
		if *folder == "" {
			*folder = c.General.Basefolder
		}
	}
	return c
}

// Save writes the configuration back as TOML.
func Save(path string, cfg Config) error {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o600)
}

// Validate reports configuration that cannot work.
func (c Config) Validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q is not one of debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q is not one of text, json", c.Log.Format)
	}
	if c.General.ReloadInterval < 1 {
		return errors.New("general.reloadInterval has to be at least 1")
	}
	if c.General.Basefolder != "" {
		if !filepath.IsAbs(c.General.Basefolder) {
			return fmt.Errorf("general.basefolder %q has to be an absolute path",
				c.General.Basefolder)
		}
		if err := checkFolder("general.basefolder", c.General.Basefolder); err != nil {
			return err
		}
	}
	if !c.FTP.Enabled && !c.FTPS.Enabled && !c.SFTP.Enabled &&
		!c.HTTP.Enabled && !c.HTTPS.Enabled && !c.TFTP.Enabled &&
		!c.General.AdminInterfaceEnabled {
		return errors.New("no server is enabled, nothing to do")
	}
	if c.General.AdminInterfaceEnabled {
		if err := c.General.validateAdmin(); err != nil {
			return err
		}
	}
	if c.FTP.Enabled || c.FTPS.Enabled {
		if err := c.validateFTP(); err != nil {
			return err
		}
	}
	if c.SFTP.Enabled {
		if err := c.SFTP.validate(); err != nil {
			return err
		}
	}
	if c.HTTP.Enabled || c.HTTPS.Enabled {
		if err := c.validateHTTP(); err != nil {
			return err
		}
	}
	if c.TFTP.Enabled {
		if err := c.TFTP.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validateHTTP checks the HTTP service, whose settings straddle [http] and
// [https] the way the FTP ones straddle [ftp] and [ftps].
func (c Config) validateHTTP() error {
	if c.HTTP.Enabled {
		if err := checkPort("http.port", c.HTTP.Port); err != nil {
			return err
		}
	}
	if c.HTTPS.Enabled {
		if err := checkPort("https.port", c.HTTPS.Port); err != nil {
			return err
		}
		if (c.HTTPS.Cert == "") != (c.HTTPS.Key == "") {
			return errors.New("https.cert and https.key have to be set together")
		}
	}
	h := c.HTTP
	if h.MaxConnections < 1 {
		return errors.New("http.maxConnections has to be at least 1")
	}
	if h.MaxUploadSize < 0 {
		return errors.New("http.maxUploadSize cannot be negative")
	}
	if h.Realm == "" {
		return errors.New("http.realm is not set")
	}
	for i, pattern := range h.PathsRequireAuth {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("http.pathsRequireAuth[%d]: %w", i, err)
		}
	}
	for i, entry := range h.Cleanup {
		if entry.Path == "" {
			return fmt.Errorf("http.cleanup[%d] has no path", i)
		}
		if entry.Keep < 0 {
			return fmt.Errorf("http.cleanup[%d].keep cannot be negative", i)
		}
	}
	for i, user := range h.Users {
		if user.Username == "" {
			return fmt.Errorf("http.users[%d] has no username", i)
		}
		if user.Password == "" {
			return fmt.Errorf("http.users[%d] %q has no password", i, user.Username)
		}
		for k, pattern := range user.Paths {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("http.users[%d].paths[%d]: %w", i, k, err)
			}
		}
	}
	return checkFolder("http.basefolder", h.Basefolder)
}

// validateFTP checks the FTP service, whose settings straddle [ftp] and [ftps].
// Everything but the two ports is shared, so it is checked whenever either
// listener is enabled.
func (c Config) validateFTP() error {
	if c.FTP.Enabled {
		if err := checkPort("ftp.port", c.FTP.Port); err != nil {
			return err
		}
	}
	if c.FTPS.Enabled {
		if err := checkPort("ftps.port", c.FTPS.Port); err != nil {
			return err
		}
		if (c.FTPS.Cert == "") != (c.FTPS.Key == "") {
			return errors.New("ftps.cert and ftps.key have to be set together")
		}
	}
	f := c.FTP
	if f.MaxConnections < 1 {
		return errors.New("ftp.maxConnections has to be at least 1")
	}
	if f.MinDataPort < 1 || f.MinDataPort > 65535 {
		return fmt.Errorf("ftp.minDataPort %d is out of range", f.MinDataPort)
	}
	if f.MinDataPort+f.MaxConnections > 65535 {
		return errors.New("ftp.minDataPort plus ftp.maxConnections exceeds the port range")
	}
	if f.MaxCommandLength < 16 {
		return errors.New("ftp.maxCommandLength has to be at least 16")
	}
	if err := checkFolder("ftp.basefolder", f.Basefolder); err != nil {
		return err
	}
	for i, user := range f.Users {
		if user.Username == "" {
			return fmt.Errorf("ftp.users[%d] has no username", i)
		}
		if user.Basefolder != "" {
			if err := checkFolder(fmt.Sprintf("ftp.users[%d].basefolder", i), user.Basefolder); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s SFTP) validate() error {
	if err := checkPort("sftp.port", s.Port); err != nil {
		return err
	}
	if s.MaxConnections < 1 {
		return errors.New("sftp.maxConnections has to be at least 1")
	}
	// The host key is a value in this file, not a path, so a broken one can be
	// reported here rather than at the first start.
	if s.HostKey != "" {
		if _, err := DecodeHostKey(s.HostKey); err != nil {
			return fmt.Errorf("sftp.hostkey: %w", err)
		}
	}
	if err := checkFolder("sftp.basefolder", s.Basefolder); err != nil {
		return err
	}
	for i, user := range s.Users {
		if user.Username == "" {
			return fmt.Errorf("sftp.users[%d] has no username", i)
		}
		if user.Basefolder != "" {
			if err := checkFolder(fmt.Sprintf("sftp.users[%d].basefolder", i), user.Basefolder); err != nil {
				return err
			}
		}
		// the keys are parsed here as well as at startup, so that -check
		// reports a key that would stop the server rather than passing it
		for k, entry := range user.AuthorizedKeys {
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(entry)); err != nil {
				return fmt.Errorf("sftp.users[%d].authorizedKeys[%d]: %w", i, k, err)
			}
		}
		if user.Password == "" && len(user.AuthorizedKeys) == 0 {
			return fmt.Errorf("sftp.users[%d] %q has neither a password nor an authorized key, "+
				"so it could never log in", i, user.Username)
		}
	}
	return nil
}

func (t TFTP) validate() error {
	if err := checkPort("tftp.port", t.Port); err != nil {
		return err
	}
	switch t.Type {
	case "", "udp4", "udp6":
	default:
		return fmt.Errorf("tftp.type %q is not one of udp4, udp6", t.Type)
	}
	if t.MaxConnections < 1 {
		return errors.New("tftp.maxConnections has to be at least 1")
	}
	if t.MaxConnectionsPerHost < 1 {
		return errors.New("tftp.maxConnectionsPerHost has to be at least 1")
	}
	if t.Timeout < 1 {
		return errors.New("tftp.timeout has to be at least 1")
	}
	if t.MaxTimeout < t.Timeout {
		return errors.New("tftp.maxTimeout has to be at least tftp.timeout")
	}
	if t.Retries < 0 {
		return errors.New("tftp.retries cannot be negative")
	}
	if t.MaxBlockSize < 8 || t.MaxBlockSize > 65464 {
		return fmt.Errorf("tftp.maxBlockSize %d is outside 8..65464", t.MaxBlockSize)
	}
	if t.MaxWindowSize < 1 {
		return errors.New("tftp.maxWindowSize has to be at least 1")
	}
	if t.MaxFileSize < 0 {
		return errors.New("tftp.maxFileSize cannot be negative")
	}
	return checkFolder("tftp.basefolder", t.Basefolder)
}

// validateAdmin checks the web interface. It edits every password in this file,
// so it may not be reachable without an account of its own.
func (g General) validateAdmin() error {
	if err := checkPort("general.adminInterfacePort", g.AdminInterfacePort); err != nil {
		return err
	}
	if g.AdminInterfaceAddress != "" && net.ParseIP(g.AdminInterfaceAddress) == nil {
		return fmt.Errorf("general.adminInterfaceAddress %q is not an address",
			g.AdminInterfaceAddress)
	}
	if g.AdminUsername == "" {
		return errors.New("general.adminUsername is not set, " +
			"the admin interface cannot be served without an account")
	}
	if g.AdminPassword == "" {
		return errors.New("general.adminPassword is not set, " +
			"the admin interface cannot be served without a password")
	}
	if (g.AdminCert == "") != (g.AdminKey == "") {
		return errors.New("general.adminCert and general.adminKey have to be set together")
	}
	return nil
}

func checkPort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d is out of range", name, port)
	}
	return nil
}

func checkFolder(name, folder string) error {
	if folder == "" {
		return fmt.Errorf("%s is not set", name)
	}
	info, err := os.Stat(folder)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a folder", name, folder)
	}
	return nil
}

func boolOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
