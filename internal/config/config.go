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
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

//go:embed template.toml
var template []byte

// Template returns the documented starter configuration.
func Template() []byte {
	return template
}

// Config is the whole configuration file.
type Config struct {
	Log  Log  `toml:"log"`
	FTP  FTP  `toml:"ftp"`
	FTPS FTPS `toml:"ftps"`
	TFTP TFTP `toml:"tftp"`
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

// Load reads path onto the defaults and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
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
	if !c.FTP.Enabled && !c.FTPS.Enabled && !c.TFTP.Enabled {
		return errors.New("neither ftp, ftps nor tftp is enabled, nothing to do")
	}
	if c.FTP.Enabled || c.FTPS.Enabled {
		if err := c.validateFTP(); err != nil {
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
