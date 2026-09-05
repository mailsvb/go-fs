// Package sftp implements the SFTP server: the SFTP subsystem of an SSH
// server, confined to a base folder and governed by the same per account
// permissions as the FTP server.
//
// The SSH transport comes from golang.org/x/crypto/ssh and the SFTP protocol
// from github.com/pkg/sftp, driven through NewRequestServer with the handlers
// in handlers.go. The alternative, sftp.NewServer, would serve the real
// filesystem with no confinement and no permission checks at all.
package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"go-fs/internal/config"
	"go-fs/internal/service"
	"go-fs/internal/vfs"
)

// Server accepts SSH connections and serves the SFTP subsystem on them.
type Server struct {
	// snapshot holds what a reload may swap. Every read goes through
	// settings(), so a live session keeps what it started under.
	snapshot atomic.Pointer[settings]
	root     *vfs.Root
	log      *slog.Logger

	ssh *ssh.ServerConfig

	listener net.Listener

	wg sync.WaitGroup

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	shutdown bool
}

// New prepares a server. The base folder, and any per user base folder, has to
// exist; every authorized key has to parse, so that a typo in one is reported
// at startup rather than silently never matching.
// settings is the part of the server a reload can replace.
type settings struct {
	cfg   config.SFTP
	users map[string]*account
}

func (s *Server) settings() *settings {
	return s.snapshot.Load()
}

// Reload swaps the accounts and the limits. The port, the folder and the host
// key cannot change under a running listener, so those report ErrNeedsRestart.
func (s *Server) Reload(cfg config.SFTP) error {
	current := s.settings().cfg
	if cfg.Enabled != current.Enabled || cfg.Port != current.Port ||
		cfg.Basefolder != current.Basefolder || cfg.HostKey != current.HostKey {
		return service.ErrNeedsRestart
	}
	users, err := buildAccounts(cfg, s.root)
	if err != nil {
		// a broken account leaves the running one in place
		return err
	}
	s.snapshot.Store(&settings{cfg: cfg, users: users})
	return nil
}

func New(cfg config.SFTP, logger *slog.Logger) (*Server, error) {
	root, err := vfs.New(cfg.Basefolder)
	if err != nil {
		return nil, fmt.Errorf("sftp.basefolder: %w", err)
	}

	users, err := buildAccounts(cfg, root)
	if err != nil {
		return nil, err
	}

	signer, err := hostKey(cfg, logger)
	if err != nil {
		return nil, err
	}

	server := &Server{
		root:  root,
		log:   logger,
		conns: make(map[net.Conn]struct{}),
	}
	server.snapshot.Store(&settings{cfg: cfg, users: users})
	server.ssh = &ssh.ServerConfig{
		PasswordCallback:  server.authenticatePassword,
		PublicKeyCallback: server.authenticatePublicKey,
		ServerVersion:     "SSH-2.0-go-fs",
	}
	server.ssh.AddHostKey(signer)
	return server, nil
}

// Start binds the listener and serves until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(s.settings().cfg.Port)))
	if err != nil {
		return err
	}
	s.listener = listener
	s.log.Info("sftp listening", "protocol", "ssh",
		"address", addressOf(listener.Addr()), "port", portOf(listener.Addr()))

	go func() {
		<-ctx.Done()
		_ = s.Shutdown(context.Background())
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.accept(ctx)
	}()
	return nil
}

// Addr reports the bound address, which is useful when port 0 was asked for.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Shutdown closes the listener and drops every open connection.
func (s *Server) Shutdown(context.Context) error {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.shutdown = true
	open := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		open = append(open, c)
	}
	s.mu.Unlock()

	if s.listener != nil {
		_ = s.listener.Close()
	}
	for _, c := range open {
		_ = c.Close()
	}
	s.wg.Wait()
	return nil
}

// Connections reports how many connections are open, for tests.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *Server) accept(ctx context.Context) {
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			s.log.Error("sftp accept failed", "error", err)
			continue
		}

		s.mu.Lock()
		if s.shutdown {
			s.mu.Unlock()
			_ = raw.Close()
			return
		}
		if len(s.conns) >= s.settings().cfg.MaxConnections {
			s.mu.Unlock()
			s.log.Debug("sftp connection refused, too many connections",
				"maxConnections", s.settings().cfg.MaxConnections)
			_ = raw.Close()
			continue
		}
		s.conns[raw] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, raw)
				s.mu.Unlock()
				_ = raw.Close()
			}()
			s.serve(raw)
		}()
	}
}

// serve runs the SSH handshake and then the session channels of one client.
func (s *Server) serve(raw net.Conn) {
	remote := raw.RemoteAddr().String()
	log := s.log.With("client", remote)

	// one snapshot for this connection, so a reload does not change the rules
	// under a live session
	set := s.settings()

	conn := raw
	if set.cfg.IdleTimeout > 0 {
		conn = &idleConn{Conn: raw, timeout: time.Duration(set.cfg.IdleTimeout) * time.Second}
	}

	handshake, chans, reqs, err := ssh.NewServerConn(conn, s.ssh)
	if err != nil {
		// a failed handshake is ordinary: a port scan, a wrong password, a
		// client that gave up
		log.Debug("sftp handshake failed", "error", err)
		return
	}
	defer func() { _ = handshake.Close() }()

	user := set.users[handshake.User()]
	if user == nil {
		// the callbacks refuse an unknown name, so this cannot normally happen
		log.Error("sftp session without an account", "user", handshake.User())
		return
	}
	log = log.With("user", user.name)
	log.Info("sftp login", "address", addressOnly(remote), "total", s.Connections())
	defer log.Info("sftp logoff", "address", addressOnly(remote))

	// global requests, keepalives among them, are answered but never acted on
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are served")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			log.Debug("sftp cannot accept the channel", "error", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveSession(channel, requests, user, log)
		}()
	}
}

// serveSession waits for the sftp subsystem request on one session channel.
// Every other request type, "shell" and "exec" in particular, is refused: this
// is a file server, not a shell host.
func (s *Server) serveSession(channel ssh.Channel, requests <-chan *ssh.Request, user *account, log *slog.Logger) {
	defer func() { _ = channel.Close() }()

	started := false
	for req := range requests {
		granted := false
		if req.Type == "subsystem" && subsystemName(req.Payload) == "sftp" && !started {
			granted = true
			started = true
		} else {
			log.Debug("sftp request refused", "type", req.Type)
		}
		if req.WantReply {
			_ = req.Reply(granted, nil)
		}
		if !granted {
			continue
		}

		server := sftp.NewRequestServer(channel, s.handlers(user, log))
		if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
			log.Debug("sftp session ended", "error", err)
		}
		_ = server.Close()
		return
	}
}

// subsystemName reads the name out of a "subsystem" request payload, which is
// one SSH string: a four byte length followed by the name.
func subsystemName(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	length := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if length < 0 || 4+length > len(payload) {
		return ""
	}
	return string(payload[4 : 4+length])
}

// idleConn closes a connection that has been silent for too long. The deadline
// is pushed forward on every read, which is the same rule the FTP control
// connection follows.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func addressOf(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP.String()
	}
	return addr.String()
}

func portOf(addr net.Addr) int {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

func addressOnly(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}
