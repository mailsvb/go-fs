// Package httpd implements the HTTP file server: browsing and downloading with
// GET, uploading with PUT and removing with DELETE, with Basic and Digest
// authentication and optional session cookies.
//
// It is a port of an Express server, so the reply shapes, the listing page and
// the authentication rules are the ones that server produced. The package is
// named httpd rather than http so that it can import net/http.
package httpd

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go-fs/internal/config"
	"go-fs/internal/service"
	"go-fs/internal/tlsconf"
	"go-fs/internal/vfs"
)

// sessionCookie is the name the Node implementation used, so that a browser
// that already holds one keeps working.
const sessionCookie = "session"

// readerPath is the legacy listing endpoint one client asks for by name.
var readerPath = regexp.MustCompile(`/dls_directory_reader\.(php|asp)$`)

// Server serves the file tree over HTTP, over TLS, or over both.
type Server struct {
	// snapshot holds what a reload may swap. Every request takes one snapshot
	// at the top of ServeHTTP and is served entirely by it.
	snapshot atomic.Pointer[settings]
	root     *vfs.Root
	log      *slog.Logger

	sessions *sessions
	// nonce is generated once and used for the life of the process, as the
	// Node implementation does.
	nonce string

	plain  net.Listener
	secure net.Listener
	server *http.Server

	wg sync.WaitGroup

	mu       sync.Mutex
	shutdown bool
}

// New prepares a server. The base folder has to exist and every configured
// regular expression has to compile, so that a typo is reported at startup
// rather than at the first request that would have matched.
// settings is the part of the server a reload can replace: the section and
// everything compiled from it.
type settings struct {
	cfg            config.HTTP
	https          config.HTTPS
	accounts       []*account
	protectedPaths []*regexp.Regexp
}

func (s *Server) settings() *settings {
	return s.snapshot.Load()
}

// newSettings compiles a section into what the request path needs.
func newSettings(cfg config.HTTP, https config.HTTPS) (*settings, error) {
	accounts, err := buildAccounts(cfg.Users)
	if err != nil {
		return nil, err
	}
	protected := make([]*regexp.Regexp, 0, len(cfg.PathsRequireAuth))
	for i, pattern := range cfg.PathsRequireAuth {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("http.pathsRequireAuth[%d]: %w", i, err)
		}
		protected = append(protected, compiled)
	}
	return &settings{cfg: cfg, https: https, accounts: accounts, protectedPaths: protected}, nil
}

// Reload swaps the accounts, the paths and the limits that are read per
// request. The ports, the folder, the certificate and the settings baked into
// the http.Server and its listener at Start report ErrNeedsRestart.
func (s *Server) Reload(cfg config.HTTP, https config.HTTPS) error {
	current := s.settings()
	if cfg.Enabled != current.cfg.Enabled || cfg.Port != current.cfg.Port ||
		cfg.Basefolder != current.cfg.Basefolder ||
		cfg.MaxConnections != current.cfg.MaxConnections ||
		cfg.ReadTimeout != current.cfg.ReadTimeout ||
		cfg.WriteTimeout != current.cfg.WriteTimeout ||
		cfg.IdleTimeout != current.cfg.IdleTimeout ||
		https.Enabled != current.https.Enabled || https.Port != current.https.Port ||
		https.Cert != current.https.Cert || https.Key != current.https.Key {
		return service.ErrNeedsRestart
	}
	next, err := newSettings(cfg, https)
	if err != nil {
		// a broken account or pattern leaves the running one in place
		return err
	}
	s.sessions.setLifetime(time.Duration(cfg.SessionTimeout) * time.Second)
	s.snapshot.Store(next)
	return nil
}

func New(cfg config.HTTP, https config.HTTPS, logger *slog.Logger) (*Server, error) {
	root, err := vfs.New(cfg.Basefolder)
	if err != nil {
		return nil, fmt.Errorf("http.basefolder: %w", err)
	}

	set, err := newSettings(cfg, https)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	server := &Server{
		root:     root,
		log:      logger,
		sessions: newSessions(time.Duration(cfg.SessionTimeout) * time.Second),
		nonce:    hex.EncodeToString(nonce),
	}
	server.snapshot.Store(set)
	server.server = &http.Server{
		Handler:      server,
		ReadTimeout:  seconds(cfg.ReadTimeout),
		WriteTimeout: seconds(cfg.WriteTimeout),
		IdleTimeout:  seconds(cfg.IdleTimeout),
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	return server, nil
}

// Start binds the listeners and serves until ctx is cancelled. The two
// listeners are independent: http.enabled serves the plain port, https.enabled
// the TLS one, and either may be on alone.
func (s *Server) Start(ctx context.Context) error {
	set := s.settings()
	if !set.cfg.Enabled && !set.https.Enabled {
		return errors.New("neither http nor https is enabled")
	}

	if set.cfg.Enabled {
		plain, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(set.cfg.Port)))
		if err != nil {
			return err
		}
		s.plain = limited(plain, set.cfg.MaxConnections)
		s.log.Info("http listening", "protocol", "http",
			"address", listenAddress(plain), "port", listenPort(plain))
	}

	if set.https.Enabled {
		tlsConfig, err := tlsconf.Build(set.https.Cert, set.https.Key, "https.cert", "https.key", s.log)
		if err != nil {
			s.closeListeners()
			return err
		}
		raw, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(set.https.Port)))
		if err != nil {
			s.closeListeners()
			return err
		}
		// the connection limit goes underneath the TLS listener: net/http
		// recognises a connection as TLS by its type, so a wrapper around the
		// *tls.Conn would leave Request.TLS empty
		secure := tls.NewListener(limited(raw, set.cfg.MaxConnections), tlsConfig)
		s.secure = secure
		s.log.Info("http listening", "protocol", "https",
			"address", listenAddress(secure), "port", listenPort(secure))
	}

	go func() {
		<-ctx.Done()
		_ = s.Shutdown(context.Background())
	}()

	for _, listener := range []net.Listener{s.plain, s.secure} {
		if listener == nil {
			continue
		}
		s.wg.Add(1)
		go func(listener net.Listener) {
			defer s.wg.Done()
			if err := s.server.Serve(listener); err != nil &&
				!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				s.log.Error("http serve failed", "error", err)
			}
		}(listener)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runCleanup(ctx)
	}()
	return nil
}

// Addr reports the bound plain address, which is useful when port 0 was asked
// for.
func (s *Server) Addr() net.Addr {
	if s.plain == nil {
		return nil
	}
	return s.plain.Addr()
}

// SecureAddr reports the bound TLS address.
func (s *Server) SecureAddr() net.Addr {
	if s.secure == nil {
		return nil
	}
	return s.secure.Addr()
}

// Shutdown closes the listeners and drops every open connection.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.shutdown = true
	s.mu.Unlock()

	_ = s.server.Close()
	s.closeListeners()
	s.wg.Wait()
	return nil
}

func (s *Server) closeListeners() {
	for _, listener := range []net.Listener{s.plain, s.secure} {
		if listener != nil {
			_ = listener.Close()
		}
	}
}

// ServeHTTP resolves the path once, authenticates, authorizes and dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// one snapshot for the whole request, so a reload halfway through cannot
	// authenticate against one account list and authorize against another
	set := s.settings()
	s.log.Debug("http request", "method", r.Method, "url", r.URL.Path, "address", addressOf(r))

	target := s.root.Resolve("/", r.URL.Path)
	if !target.Valid {
		// a path that leaves the served folder is answered as a path that is
		// not there, which is what it is from the client's side
		s.log.Debug("http path refused", "url", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	user, ok := s.authenticate(set, w, r, target.Virtual)
	if !ok {
		return
	}
	if user != nil && !user.allows(target.Virtual) {
		s.log.Debug("http path not allowed for the account",
			"user", user.name, "path", target.Virtual)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleGet(set, w, r, target)
	case http.MethodPut:
		if user != nil && !user.upload {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		s.handlePut(set, w, r, target)
	case http.MethodDelete:
		if user != nil && !user.delete {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		s.handleDelete(set, w, r, target)
	case http.MethodPost:
		if !readerPath.MatchString(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		s.handleDirectoryReader(set, w, r, target)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE, POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// limited caps how many connections a listener hands out at once.
func limited(listener net.Listener, max int) net.Listener {
	if max < 1 {
		return listener
	}
	return &limitedListener{Listener: listener, slots: make(chan struct{}, max)}
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitedConn{Conn: conn, release: func() { <-l.slots }}, nil
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func seconds(value int) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Second
}

func listenAddress(listener net.Listener) string {
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
		return tcp.IP.String()
	}
	return listener.Addr().String()
}

func listenPort(listener net.Listener) int {
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

// addressOf is the client address without its port.
func addressOf(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
