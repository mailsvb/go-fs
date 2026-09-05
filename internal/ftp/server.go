// Package ftp implements the FTP server: RFC 959 with the extensions of
// RFC 2228 (AUTH, PBSZ, PROT), RFC 2389 (FEAT, OPTS), RFC 2428 (EPRT, EPSV),
// RFC 3659 (MLST, MLSD, MDTM, SIZE, REST) and the RFC 775 X aliases.
package ftp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"go-fs/internal/config"
	"go-fs/internal/vfs"
)

// Server accepts control connections on the plain and, when configured, on the
// implicit TLS port.
type Server struct {
	cfg  config.FTP
	ftps config.FTPS
	root *vfs.Root
	log  *slog.Logger
	tls  *tls.Config

	plain  net.Listener
	secure net.Listener

	wg sync.WaitGroup

	mu       sync.Mutex
	conns    map[*conn]struct{}
	shutdown bool
}

// New prepares a server. The base folder, and any per user base folder, has to
// exist. When TLS is enabled without a certificate one is generated, so that a
// published private key never has to ship with the program.
func New(cfg config.FTP, ftps config.FTPS, logger *slog.Logger) (*Server, error) {
	root, err := vfs.New(cfg.Basefolder)
	if err != nil {
		return nil, fmt.Errorf("ftp.basefolder: %w", err)
	}
	for i, user := range cfg.Users {
		if user.Basefolder == "" {
			continue
		}
		if _, err := vfs.New(user.Basefolder); err != nil {
			return nil, fmt.Errorf("ftp.users[%d].basefolder: %w", i, err)
		}
	}

	server := &Server{
		cfg:   cfg,
		ftps:  ftps,
		root:  root,
		log:   logger,
		conns: make(map[*conn]struct{}),
	}
	if ftps.Enabled {
		server.tls, err = buildTLSConfig(ftps, logger)
		if err != nil {
			return nil, err
		}
	}
	return server, nil
}

// Start binds the listeners and serves until ctx is cancelled.
// The two listeners are independent: ftp.enabled serves the plain control
// port, ftps.enabled the implicit TLS one, and either may be on alone.
func (s *Server) Start(ctx context.Context) error {
	if !s.cfg.Enabled && !s.ftps.Enabled {
		return errors.New("neither ftp nor ftps is enabled")
	}

	if s.cfg.Enabled {
		plain, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(s.cfg.Port)))
		if err != nil {
			return err
		}
		s.plain = plain
		s.log.Info("ftp listening", "protocol", "tcp",
			"address", addressOf(plain.Addr()), "port", portOf(plain.Addr()))
	}

	if s.ftps.Enabled {
		secure, err := tls.Listen("tcp", net.JoinHostPort("", strconv.Itoa(s.ftps.Port)), s.tls)
		if err != nil {
			if s.plain != nil {
				_ = s.plain.Close()
			}
			return err
		}
		s.secure = secure
		s.log.Info("ftp listening", "protocol", "tls",
			"address", addressOf(secure.Addr()), "port", portOf(secure.Addr()))
	}

	go func() {
		<-ctx.Done()
		_ = s.Shutdown(context.Background())
	}()

	if s.plain != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.accept(ctx, s.plain, false)
		}()
	}
	if s.secure != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.accept(ctx, s.secure, true)
		}()
	}
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

// SecureAddr reports the bound implicit TLS address.
func (s *Server) SecureAddr() net.Addr {
	if s.secure == nil {
		return nil
	}
	return s.secure.Addr()
}

// Shutdown closes the listeners and drops every open connection.
func (s *Server) Shutdown(context.Context) error {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.shutdown = true
	open := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		open = append(open, c)
	}
	s.mu.Unlock()

	if s.plain != nil {
		_ = s.plain.Close()
	}
	if s.secure != nil {
		_ = s.secure.Close()
	}
	for _, c := range open {
		c.close()
	}
	s.wg.Wait()
	return nil
}

// Connections reports how many control connections are open, for tests.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *Server) accept(ctx context.Context, listener net.Listener, secure bool) {
	for {
		raw, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			s.log.Error("ftp accept failed", "error", err)
			continue
		}

		s.mu.Lock()
		if s.shutdown {
			s.mu.Unlock()
			_ = raw.Close()
			return
		}
		// maxConnections is a limit on control connections, as in the original
		if len(s.conns) >= s.cfg.MaxConnections {
			s.mu.Unlock()
			s.log.Debug("ftp connection refused, too many connections",
				"maxConnections", s.cfg.MaxConnections)
			_ = raw.Close()
			continue
		}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(ctx, raw, secure)
		}()
	}
}

func (s *Server) register(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[c] = struct{}{}
}

func (s *Server) unregister(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// listenData binds a passive data listener on the configured port range.
// Binding the real listener directly leaves no window in which the port can be
// taken by somebody else.
func (s *Server) listenData() (net.Listener, int, error) {
	maxPort := min(s.cfg.MinDataPort+s.cfg.MaxConnections, 65535)
	for port := s.cfg.MinDataPort; port <= maxPort; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
		if err == nil {
			return listener, port, nil
		}
	}
	return nil, 0, errors.New("no free data port")
}

// normalizeAddress strips the IPv4 mapped IPv6 prefix, so addresses compare and
// print the same regardless of the socket family they arrived on.
func normalizeAddress(address string) string {
	return strings.TrimPrefix(address, "::ffff:")
}

func hostOf(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return normalizeAddress(addr.String())
	}
	return normalizeAddress(host)
}

func addressOf(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return normalizeAddress(tcp.IP.String())
	}
	return hostOf(addr)
}

func portOf(addr net.Addr) int {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

func isLoopback(address string) bool {
	ip := net.ParseIP(normalizeAddress(address))
	return ip != nil && ip.IsLoopback()
}
