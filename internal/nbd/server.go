//go:build !light && !no_nbd

// Package nbd is the NBD server: it serves the disk images of the Disks page
// (see internal/disks) to QEMU and other NBD clients, one image to a
// connection, the image's file name being the export name.
//
// Only fixed newstyle negotiation is spoken, and only over TLS: before
// NBD_OPT_STARTTLS every option is refused. The client has to show a
// certificate that nbd.clientCA issued and nbd.clientCRL does not list, whose
// common name is the username of an account that sets nbd. That account's
// allowUserFileRetrieve lets it attach an image read-only, and
// allowUserFileOverwrite as well read-write; only one client at a time may
// have an image attached read-write.
//
// The transmission phase takes requests in order and runs up to sixteen of
// them at once. Writes go to the file as they come; FLUSH and FUA sync it;
// TRIM and WRITE_ZEROES punch holes, so space the guest frees goes back to the
// filesystem; BLOCK_STATUS reports the holes through base:allocation; and a
// full filesystem is reported as ENOSPC, which lets QEMU pause the VM rather
// than fail its disk.
package nbd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/disks"
	"github.com/mailsvb/go-fs/internal/service"
)

// Server accepts NBD connections.
type Server struct {
	snapshot atomic.Pointer[settings]
	folder   *disks.Folder
	log      *slog.Logger

	// wrap, when set, stands in for an attached image, which is how a test
	// makes the disk full
	wrap func(image) image

	listener net.Listener
	wg       sync.WaitGroup

	mu       sync.Mutex
	conns    map[*conn]struct{}
	shutdown bool
}

// settings is the part of the server a reload can replace.
type settings struct {
	cfg      config.NBD
	accounts map[string]account
	cert     tls.Certificate
	roots    *x509.CertPool
	// revoked are the serials nbd.clientCRL lists, by the issuer they are
	// serials of
	revoked map[string]bool
	allowed []netip.Prefix
}

// account is what an NBD account may do.
type account struct {
	name     string
	writable bool
}

func (s *Server) settings() *settings { return s.snapshot.Load() }

// New prepares a server. users are the [[users]] entries that set nbd, which
// the supervisor hands over already filtered.
func New(cfg config.NBD, users []config.User, logger *slog.Logger) (*Server, error) {
	if cfg.DisksFolder == "" {
		return nil, errors.New("nbd needs general.baseFolderDisks")
	}
	folder, err := disks.Open(cfg.DisksFolder, logger)
	if err != nil {
		return nil, fmt.Errorf("general.baseFolderDisks: %w", err)
	}
	set, err := newSettings(cfg, users)
	if err != nil {
		return nil, err
	}
	s := &Server{folder: folder, log: logger, conns: map[*conn]struct{}{}}
	s.snapshot.Store(set)
	s.warn(set)
	return s, nil
}

func newSettings(cfg config.NBD, users []config.User) (*settings, error) {
	certPEM, keyPEM, err := pair(cfg)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("nbd.cert and nbd.key: %w", err)
	}
	authorities, err := config.ParseCertificateAuthorities(cfg.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("nbd.clientCA: %w", err)
	}
	roots := x509.NewCertPool()
	for _, authority := range authorities {
		roots.AddCert(authority)
	}
	revoked := map[string]bool{}
	if cfg.ClientCRL != "" {
		list, err := config.ParseCRL(cfg.ClientCRL, authorities)
		if err != nil {
			return nil, fmt.Errorf("nbd.clientCRL: %w", err)
		}
		// a certificate the CA issued names it as its issuer byte for byte,
		// which is how a chain is built at all
		for _, serial := range list.Serials {
			revoked[revocationKey(list.Authority.RawSubject, serial.Bytes())] = true
		}
	}
	var allowed []netip.Prefix
	for i, entry := range cfg.AllowedClients {
		prefix, err := config.ParseProxy(entry)
		if err != nil {
			return nil, fmt.Errorf("nbd.allowedClients[%d]: %w", i, err)
		}
		allowed = append(allowed, prefix)
	}
	accounts := map[string]account{}
	for _, user := range users {
		perms := user.Permissions()
		if !user.NBD || !perms.FileRetrieve {
			continue
		}
		accounts[user.Username] = account{name: user.Username, writable: perms.FileOverwrite}
	}
	return &settings{cfg: cfg, accounts: accounts, cert: cert, roots: roots, revoked: revoked, allowed: allowed}, nil
}

func pair(cfg config.NBD) (certPEM, keyPEM []byte, err error) {
	if cfg.Cert == "" || cfg.Key == "" {
		return nil, nil, errors.New("nbd.cert and nbd.key are required")
	}
	if certPEM, err = config.DecodeCertificate(cfg.Cert); err != nil {
		return nil, nil, fmt.Errorf("nbd.cert: %w", err)
	}
	if keyPEM, err = config.DecodePrivateKey(cfg.Key); err != nil {
		return nil, nil, fmt.Errorf("nbd.key: %w", err)
	}
	return certPEM, keyPEM, nil
}

// revocationKey names a serial of an issuer, since two CAs of nbd.clientCA
// may well hand out the same one.
func revocationKey(rawIssuer, serial []byte) string {
	return hex.EncodeToString(rawIssuer) + "/" + hex.EncodeToString(serial)
}

// warn says what will fail before long: a certificate that runs out, and a
// revocation list past its next update, which a CA that publishes them on
// schedule has replaced by now.
func (s *Server) warn(set *settings) {
	if leaf := set.cert.Leaf; leaf != nil && time.Until(leaf.NotAfter) < 30*24*time.Hour {
		s.log.Warn("the nbd certificate runs out soon, or has", "expires", leaf.NotAfter.Format(time.DateOnly))
	}
	if set.cfg.ClientCRL == "" {
		return
	}
	authorities, err := config.ParseCertificateAuthorities(set.cfg.ClientCA)
	if err != nil {
		return
	}
	if list, err := config.ParseCRL(set.cfg.ClientCRL, authorities); err == nil &&
		!list.NextUpdate.IsZero() && time.Now().After(list.NextUpdate) {
		s.log.Warn("nbd.clientCRL is past its next update; a certificate revoked since is still accepted",
			"nextUpdate", list.NextUpdate.Format(time.DateOnly))
	}
}

// Reload swaps the accounts, the certificates, the revocation list and the
// limits. A connection that the new settings would refuse is closed: one
// whose account is gone or lost the nbd switch, one attached read-write by an
// account that lost the right to write, and one whose certificate is revoked
// or no longer issued by a CA of nbd.clientCA. The port, the address and the
// folder report ErrNeedsRestart.
func (s *Server) Reload(cfg config.NBD, users []config.User) error {
	current := s.settings()
	if cfg.Enabled != current.cfg.Enabled || cfg.Port != current.cfg.Port ||
		cfg.Address != current.cfg.Address || cfg.DisksFolder != current.cfg.DisksFolder ||
		cfg.KeepAlive != current.cfg.KeepAlive {
		return service.ErrNeedsRestart
	}
	next, err := newSettings(cfg, users)
	if err != nil {
		return err
	}
	s.snapshot.Store(next)
	s.warn(next)

	s.mu.Lock()
	open := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		open = append(open, c)
	}
	s.mu.Unlock()
	for _, c := range open {
		if reason := next.refuses(c.identity()); reason != "" {
			c.log.Info("nbd connection closed, the reloaded configuration refuses it", "reason", reason)
			c.close("the configuration no longer admits it: " + reason)
		}
	}
	return nil
}

// identity is who a connection logged in as.
type identity struct {
	cert     *x509.Certificate
	account  string
	writable bool
}

// refuses says why the settings would not admit a connection that logged in
// as id, empty when they would. A connection that has not logged in yet is
// judged when it does.
func (set *settings) refuses(id identity) string {
	if id.cert == nil {
		return ""
	}
	if set.revoked[revocationKey(id.cert.RawIssuer, id.cert.SerialNumber.Bytes())] {
		return "its certificate is revoked"
	}
	if _, err := id.cert.Verify(x509.VerifyOptions{Roots: set.roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "its certificate is not accepted: " + err.Error()
	}
	user, ok := set.accounts[id.account]
	if !ok {
		return fmt.Sprintf("there is no account %q that sets nbd and allowUserFileRetrieve", id.account)
	}
	if id.writable && !user.writable {
		return fmt.Sprintf("the account %q may no longer write", id.account)
	}
	return ""
}

// verify is what the TLS handshake checks once the chain is verified: that
// the certificate is not revoked, and that it names an account.
func (set *settings) verify(state tls.ConnectionState) (identity, error) {
	if len(state.PeerCertificates) == 0 {
		return identity{}, errors.New("the client showed no certificate")
	}
	leaf := state.PeerCertificates[0]
	if set.revoked[revocationKey(leaf.RawIssuer, leaf.SerialNumber.Bytes())] {
		return identity{}, fmt.Errorf("the certificate %s of %q is revoked", leaf.SerialNumber.Text(16), leaf.Subject.CommonName)
	}
	user, ok := set.accounts[leaf.Subject.CommonName]
	if !ok {
		return identity{}, fmt.Errorf("the certificate names %q, which is no account that sets nbd and allowUserFileRetrieve",
			leaf.Subject.CommonName)
	}
	return identity{cert: leaf, account: user.name, writable: user.writable}, nil
}

// tlsConfig is the TLS server side of one connection, built from the
// settings of the moment it switches to TLS. verified is told who the client
// is once the handshake has checked it.
func (set *settings) tlsConfig(verified func(identity)) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{set.cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    set.roots,
		MinVersion:   tls.VersionTLS12,
		VerifyConnection: func(state tls.ConnectionState) error {
			id, err := set.verify(state)
			if err != nil {
				return err
			}
			verified(id)
			return nil
		},
	}
}

// allows reports whether a connection from addr may be served.
func (set *settings) allows(addr net.Addr) bool {
	if len(set.allowed) == 0 {
		return true
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	for _, prefix := range set.allowed {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// Start binds the listener and serves until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	cfg := s.settings().cfg
	lc := net.ListenConfig{KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: time.Duration(cfg.KeepAlive) * time.Second, Interval: 5 * time.Second, Count: 3,
	}}
	listener, err := lc.Listen(ctx, "tcp", net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port)))
	if err != nil {
		return err
	}
	s.listener = listener
	addr := listener.Addr().(*net.TCPAddr)
	s.log.Info("nbd listening", "protocol", "nbd", "address", addr.IP.String(), "port", addr.Port,
		"disks", s.folder.Base())

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

// Folder is the folder of the disk images the server serves.
func (s *Server) Folder() *disks.Folder { return s.folder }

// Shutdown closes the listener and every connection. An attached image is
// synced before its connection lets go of it.
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
	if s.listener != nil {
		_ = s.listener.Close()
	}
	for _, c := range open {
		c.close("the server is shutting down")
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
			s.log.Error("nbd accept failed", "error", err)
			continue
		}
		set := s.settings()
		remote := raw.RemoteAddr().String()
		if !set.allows(raw.RemoteAddr()) {
			s.log.Info("nbd connection refused, the address is not in nbd.allowedClients", "client", remote)
			_ = raw.Close()
			continue
		}
		s.mu.Lock()
		if s.shutdown {
			s.mu.Unlock()
			_ = raw.Close()
			return
		}
		if len(s.conns) >= set.cfg.MaxConnections {
			s.mu.Unlock()
			s.log.Info("nbd connection refused, too many connections",
				"client", remote, "maxConnections", set.cfg.MaxConnections)
			_ = raw.Close()
			continue
		}
		c := newConn(s, raw)
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			c.serve()
		}()
	}
}

// fingerprint is the SHA-256 of a certificate, hex.
func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
