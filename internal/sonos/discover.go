package sonos

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/mailsvb/go-fs/internal/logging"
)

// ssdpGroup is where SSDP searches go.
const ssdpGroup = "239.255.255.250:1900"

// zonePlayer is the device type of a Sonos speaker.
const zonePlayer = "urn:schemas-upnp-org:device:ZonePlayer:1"

// Player is one speaker.
type Player struct {
	// UUID is the speaker's id, RINCON_ and its MAC address and port.
	UUID string `json:"uuid"`
	// Room is the name of the room the Sonos app gave it.
	Room string `json:"room"`
	// Model is what it is, Sonos One or Era 100.
	Model string `json:"model,omitempty"`
	// Host is where its services are, ip:1400.
	Host string `json:"-"`
}

// Discover asks the network for its speakers and waits for their answers
// for as long as wait. The search goes out on every interface that takes
// multicast, since the speakers may be on a network other than the one the
// default route leads to. A speaker found is described before it is
// returned, so a device that only says it is one is left out.
func (c *Client) Discover(ctx context.Context, wait time.Duration) ([]Player, error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, fmt.Errorf("sonos search: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()

	to, err := net.ResolveUDPAddr("udp4", c.Search)
	if err != nil {
		return nil, fmt.Errorf("sonos search: %w", err)
	}
	search := []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + ssdpGroup + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 1\r\n" +
		"ST: " + zonePlayer + "\r\n\r\n")
	log := logging.From(ctx, logging.Discard)
	// a datagram may be lost, so the search goes out twice
	if err := sendSearch(conn, to, search, log); err != nil {
		return nil, err
	}
	resend := time.AfterFunc(wait/3, func() { _ = sendSearch(conn, to, search, log) })
	defer resend.Stop()

	_ = conn.SetReadDeadline(time.Now().Add(wait))
	locations := map[string]string{} // by host, so a speaker answering twice counts once
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		location, ok := answerLocation(buf[:n], from, c.port)
		log.Debug("ssdp <", "from", from.String(), "bytes", n, "speaker", ok, "location", location)
		if ok {
			locations[from.(*net.UDPAddr).IP.String()] = location
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	players := make([]Player, 0, len(locations))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, location := range locations {
		wg.Go(func() {
			player, err := c.describe(ctx, location)
			if err != nil {
				// a device that answered the search but cannot be
				// described is left out, which only the trace says
				log.Debug("sonos speaker left out, its description cannot be read",
					"location", location, "error", err)
				return
			}
			mu.Lock()
			players = append(players, player)
			mu.Unlock()
		})
	}
	wg.Wait()
	return players, nil
}

// sendSearch sends the search to the SSDP group once over every interface
// that takes multicast, or to a single address as it is.
func sendSearch(conn net.PacketConn, to *net.UDPAddr, search []byte, log *slog.Logger) error {
	if !to.IP.IsMulticast() {
		_, err := conn.WriteTo(search, to)
		log.Debug("ssdp >", "to", to.String(), "bytes", len(search), "error", err)
		return err
	}
	pc := ipv4.NewPacketConn(conn)
	interfaces, _ := net.Interfaces()
	sent := false
	for _, ifi := range interfaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if !hasIPv4(ifi) {
			continue
		}
		if pc.SetMulticastInterface(&ifi) != nil {
			continue
		}
		_, err := pc.WriteTo(search, nil, to)
		log.Debug("ssdp >", "to", to.String(), "interface", ifi.Name, "bytes", len(search), "error", err)
		if err == nil {
			sent = true
		}
	}
	if sent {
		return nil
	}
	// no interface took it by name; the default route may still
	_, err := conn.WriteTo(search, to)
	log.Debug("ssdp >", "to", to.String(), "interface", "default route", "bytes", len(search), "error", err)
	if err != nil {
		return fmt.Errorf("sonos search: %w", err)
	}
	return nil
}

func hasIPv4(ifi net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return true
		}
	}
	return false
}

// answerLocation reads an answer to the search and returns where the
// description of the speaker is. Only a description on the address the
// answer came from, at the port of the speakers, is believed: anyone on the
// network can answer, and the server would otherwise fetch whatever they
// name.
func answerLocation(datagram []byte, from net.Addr, port string) (string, bool) {
	res, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(datagram)), nil)
	if err != nil {
		return "", false
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.EqualFold(res.Header.Get("ST"), zonePlayer) {
		return "", false
	}
	udp, ok := from.(*net.UDPAddr)
	if !ok {
		return "", false
	}
	location, err := url.Parse(res.Header.Get("LOCATION"))
	if err != nil || location.Scheme != "http" || location.Port() != port {
		return "", false
	}
	ip := net.ParseIP(location.Hostname())
	if ip == nil || !ip.Equal(udp.IP) {
		return "", false
	}
	return location.String(), true
}

// Probe describes the speaker at host, an address with or without its port,
// for the speakers the configuration names where a search does not reach.
func (c *Client) Probe(ctx context.Context, host string) (Player, error) {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), Port)
	}
	return c.describe(ctx, "http://"+host+"/xml/device_description.xml")
}

// describe reads the description of a speaker.
func (c *Client) describe(ctx context.Context, location string) (Player, error) {
	where, err := url.Parse(location)
	if err != nil {
		return Player{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return Player{}, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return Player{}, fmt.Errorf("sonos description: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return Player{}, fmt.Errorf("sonos description: %s answered %s", where.Host, res.Status)
	}
	var desc struct {
		Device struct {
			DeviceType string `xml:"deviceType"`
			ModelName  string `xml:"modelName"`
			UDN        string `xml:"UDN"`
			RoomName   string `xml:"roomName"`
		} `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(res.Body, maxReply)).Decode(&desc); err != nil {
		return Player{}, fmt.Errorf("sonos description: %w", err)
	}
	if desc.Device.DeviceType != zonePlayer {
		return Player{}, errors.New("sonos description: " + where.Host + " is no Sonos speaker")
	}
	return Player{
		UUID:  strings.TrimPrefix(desc.Device.UDN, "uuid:"),
		Room:  desc.Device.RoomName,
		Model: desc.Device.ModelName,
		Host:  where.Host,
	}, nil
}
