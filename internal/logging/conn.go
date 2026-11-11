package logging

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Conn is c with its opening and its closing recorded at debug: "conn open"
// with both ends, and "conn closed" with the bytes read and written and how
// long it was open. It is for a connection that no protocol above it traces,
// such as the one an SSH or SMB client runs over. What travels over it is
// counted, never recorded.
func Conn(log *slog.Logger, peer string, c net.Conn) net.Conn {
	log = OrDiscard(log)
	if !log.Enabled(context.Background(), slog.LevelDebug) {
		return c
	}
	log = log.With("peer", peer, "local", c.LocalAddr().String(), "remote", c.RemoteAddr().String())
	log.Debug("conn open")
	return &countedConn{Conn: c, log: log, opened: time.Now()}
}

type countedConn struct {
	net.Conn
	log         *slog.Logger
	opened      time.Time
	read, wrote atomic.Int64
	once        sync.Once
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.wrote.Add(int64(n))
	return n, err
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		attrs := []slog.Attr{
			slog.Int64("bytesIn", c.read.Load()),
			slog.Int64("bytesOut", c.wrote.Load()),
			slog.Duration("took", time.Since(c.opened).Round(time.Millisecond)),
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
		}
		c.log.LogAttrs(context.Background(), slog.LevelDebug, "conn closed", attrs...)
	})
	return err
}

// TLS is what a record says about an established TLS connection: the
// version, the cipher suite, the server name asked for and the protocol
// agreed on.
func TLS(state *tls.ConnectionState) slog.Value {
	if state == nil {
		return slog.StringValue("none")
	}
	attrs := []slog.Attr{
		slog.String("version", tls.VersionName(state.Version)),
		slog.String("cipher", tls.CipherSuiteName(state.CipherSuite)),
		slog.Bool("resumed", state.DidResume),
	}
	if state.ServerName != "" {
		attrs = append(attrs, slog.String("serverName", state.ServerName))
	}
	if state.NegotiatedProtocol != "" {
		attrs = append(attrs, slog.String("alpn", state.NegotiatedProtocol))
	}
	if len(state.PeerCertificates) > 0 {
		attrs = append(attrs, slog.String("peer", state.PeerCertificates[0].Subject.String()))
	}
	return slog.GroupValue(attrs...)
}
