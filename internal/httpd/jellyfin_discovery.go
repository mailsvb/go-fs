//go:build !light && !no_jellyfin

package httpd

import (
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
)

// A Jellyfin client looks for servers on the local network by a broadcast to
// UDP port 7359 that asks "who is JellyfinServer?", and every server answers
// the client with its address, id and name. With http.jellyfinDiscovery on,
// and while there is a jellyfin repository, this server answers as well.

// jellyfinDiscoveryPort is the port the clients broadcast to.
const jellyfinDiscoveryPort = 7359

// jellyfinDiscoveryQuestion is what a client asks, compared ignoring case.
const jellyfinDiscoveryQuestion = "who is jellyfinserver?"

// jellyfinDiscovery is the socket the questions arrive on, while there is one.
type jellyfinDiscovery struct {
	mu sync.Mutex
	// port is jellyfinDiscoveryPort; a test asks for any.
	port int
	conn net.PacketConn
	// stopped is closed when the loop that answers on conn has returned.
	stopped chan struct{}
}

// discoverJellyfin opens the socket the clients ask on, or closes it, as the
// settings say. A port that is taken, by a Jellyfin server on the same host
// for one, is warned about and leaves the rest of the server running.
func (s *Server) discoverJellyfin(set *settings) {
	d := s.jellyfin.discovery
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
	}
	if !set.jellyfin || !set.cfg.JellyfinDiscovery {
		d.stop()
		return
	}
	if d.conn != nil {
		return
	}
	// a broadcast reaches a socket bound to every interface and none bound to
	// one address, so only the loopback address, from which no other host is
	// answered anyway, is kept to
	address := ""
	if isLoopback(set.cfg.Address) {
		address = set.cfg.Address
		s.log.Warn("http.jellyfinDiscovery is on, but http.address is the loopback one, "+
			"so no client on another host finds this server", "address", address)
	}
	conn, err := net.ListenPacket("udp", net.JoinHostPort(address, strconv.Itoa(d.port)))
	if err != nil {
		s.log.Warn("http.jellyfinDiscovery is on, but Jellyfin clients cannot be answered", "error", err)
		return
	}
	s.log.Info("jellyfin discovery listening", "address", conn.LocalAddr().String())
	d.conn, d.stopped = conn, make(chan struct{})
	go s.answerDiscovery(conn, d.stopped)
}

// stopJellyfinDiscovery closes the socket, for good once the server shuts
// down.
func (s *Server) stopJellyfinDiscovery() {
	d := s.jellyfin.discovery
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stop()
}

// stop closes the socket and waits for its loop to return. d.mu is held.
func (d *jellyfinDiscovery) stop() {
	if d.conn == nil {
		return
	}
	_ = d.conn.Close()
	<-d.stopped
	d.conn, d.stopped = nil, nil
}

// discoveryAddr is the address the socket is bound to, nil while there is
// none.
func (s *Server) discoveryAddr() net.Addr {
	d := s.jellyfin.discovery
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil
	}
	return d.conn.LocalAddr()
}

// answerDiscovery answers every question that arrives on conn until it is
// closed.
func (s *Server) answerDiscovery(conn net.PacketConn, stopped chan struct{}) {
	defer close(stopped)
	buf := make([]byte, 512)
	for {
		n, client, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Debug("jellyfin discovery read failed", "error", err)
			continue
		}
		if !strings.Contains(strings.ToLower(string(buf[:n])), jellyfinDiscoveryQuestion) {
			continue
		}
		set := s.settings()
		address := s.discoveryAddress(set, client)
		if address == "" {
			s.log.Debug("jellyfin discovery found no address to answer with", "client", client.String())
			continue
		}
		answer, _ := json.Marshal(map[string]any{
			"Address":         address,
			"Id":              s.jellyfinID("server"),
			"Name":            s.jellyfinServerName(set),
			"EndpointAddress": nil,
		})
		if _, err := conn.WriteTo(answer, client); err != nil {
			s.log.Debug("jellyfin discovery answer failed", "client", client.String(), "error", err)
			continue
		}
		s.log.Debug("jellyfin discovery answered", "client", client.String(), "address", address)
	}
}

// discoveryAddress is the address a client is told to reach the server at:
// the url of a jellyfin repository, or else the plain listener, or the TLS one
// when there is no plain one, at http.address or, where that binds every
// interface, at the address of the interface the client is reached through.
func (s *Server) discoveryAddress(set *settings, client net.Addr) string {
	if set.cfg.JellyfinURL != "" {
		return set.cfg.JellyfinURL
	}
	scheme, listener := "http", s.plain
	if listener == nil {
		scheme, listener = "https", s.secure
	}
	if listener == nil {
		return ""
	}
	host := set.cfg.Address
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = localAddressTowards(client)
		if host == "" {
			return ""
		}
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(listenPort(listener)))
}

// localAddressTowards is the address of the interface this host reaches a
// client through, "" when it cannot. Connecting a UDP socket only picks the
// route; nothing is sent.
func localAddressTowards(client net.Addr) string {
	conn, err := net.Dial("udp", client.String())
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	if local, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return local.IP.String()
	}
	return ""
}
