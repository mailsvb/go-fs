package ftp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

var (
	errNoDataChannel  = errors.New("no data channel")
	errDataTimeout    = errors.New("the data connection was not established")
	errDataConnection = errors.New("the data connection failed")
)

func (c *conn) dataTimeout() time.Duration {
	return time.Duration(c.set.cfg.DataTimeout) * time.Second
}

// closeData drops the passive listener and any connection waiting on it.
func (c *conn) closeData() {
	if c.passive != nil {
		_ = c.passive.Close()
		c.passive = nil
	}
	if c.dataCh != nil {
		select {
		case waiting := <-c.dataCh:
			if waiting != nil {
				_ = waiting.Close()
			}
		default:
		}
		c.dataCh = nil
	}
}

// listenPassive binds a passive data listener and starts accepting the one
// connection it is meant to take.
func (c *conn) listenPassive() (int, error) {
	c.closeData()
	c.mode = dataNone

	listener, port, err := c.server.listenData(c.set)
	if err != nil {
		return 0, err
	}
	c.passive = listener
	c.mode = dataPassive
	c.dataCh = make(chan net.Conn, 1)

	channel := c.dataCh
	go func() {
		for {
			accepted, err := listener.Accept()
			if err != nil {
				return
			}
			// Only the client that asked for the data channel may use it,
			// otherwise a third party could read or inject transfer data.
			if !c.set.cfg.AllowForeignDataConnection && !c.isSamePeer(hostOf(accepted.RemoteAddr())) {
				c.log.Debug("ftp rejected data connection", "from", hostOf(accepted.RemoteAddr()))
				_ = accepted.Close()
				continue
			}
			// stop accepting further connections on this passive port
			_ = listener.Close()
			c.log.Debug("ftp data connection established", "from", hostOf(accepted.RemoteAddr()))
			select {
			case channel <- accepted:
			default:
				_ = accepted.Close()
			}
			return
		}
	}()
	return port, nil
}

// openData returns the data connection for a transfer.
func (c *conn) openData() (net.Conn, error) {
	switch c.mode {
	case dataActive:
		c.log.Debug("ftp opening active data connection",
			"address", c.activeHost, "port", c.activePort,
			"secure", c.secure.get(), "protected", c.protected)
		dialer := net.Dialer{Timeout: c.dataTimeout()}
		dialed, err := dialer.Dial("tcp", net.JoinHostPort(c.activeHost, strconv.Itoa(c.activePort)))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errDataConnection, err)
		}
		return c.secureData(dialed), nil

	case dataPassive:
		select {
		case accepted := <-c.dataCh:
			return c.secureData(accepted), nil
		case <-time.After(c.dataTimeout()):
			return nil, errDataTimeout
		}
	}
	return nil, errNoDataChannel
}

// secureData wraps a data connection when PROT P is in effect.
func (c *conn) secureData(raw net.Conn) net.Conn {
	if !c.secure.get() || !c.protected || c.server.tls == nil {
		return raw
	}
	c.log.Debug("ftp data connection is secure")
	return tls.Server(raw, c.server.tls)
}

// withData runs work over a fresh data connection and answers the client. work
// returns the reply for a successful transfer.
func (c *conn) withData(opening string, work func(data net.Conn) (code, message string)) {
	if c.mode == dataNone {
		c.reply("501", "Command failed")
		return
	}
	if opening == "" {
		opening = "Opening data channel"
	}
	c.reply("150", opening)

	data, err := c.openData()
	// a data channel is used once, the client asks for a new one per transfer
	c.mode = dataNone
	c.closeData()
	if err != nil {
		c.log.Debug("ftp data connection failed", "error", err)
		c.reply("425", "Cannot open the data connection")
		return
	}

	_ = data.SetDeadline(time.Now().Add(dataDeadline))
	c.beginTransfer(data)
	code, message := work(data)
	_ = data.Close()
	aborted := c.endTransfer()

	if aborted {
		c.reply("426", "Connection closed; transfer aborted")
		c.reply("226", "Abort successful")
		return
	}
	c.reply(code, message)
}

// dataDeadline bounds a single transfer. It is generous, the data timeout only
// covers establishing the connection.
const dataDeadline = 24 * time.Hour

// isDataTargetAllowed reports whether an active data connection may be opened.
//
// An active data connection may only target the client that issued the command,
// otherwise the server can be abused to reach third parties on the client's
// behalf (RFC 2577).
func (c *conn) isDataTargetAllowed(address string, port int) bool {
	if port < 1024 || port > 65535 {
		return false
	}
	return c.set.cfg.AllowFtpBounce || c.isSamePeer(address)
}

// cmdPort handles PORT.
func cmdPort(c *conn, arg string) {
	if c.epsvAll {
		c.reply("501", "EPSV ALL in effect")
		return
	}
	c.mode = dataNone
	c.closeData()

	parts := strings.Split(arg, ",")
	if len(parts) != 6 {
		c.reply("501", "Port command failed")
		return
	}
	numbers := make([]int, 6)
	for i, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || value < 0 || value > 255 {
			c.reply("501", "Port command failed")
			return
		}
		numbers[i] = value
	}
	address := fmt.Sprintf("%d.%d.%d.%d", numbers[0], numbers[1], numbers[2], numbers[3])
	port := numbers[4]*256 + numbers[5]

	if !c.isDataTargetAllowed(address, port) {
		c.reply("501", "Port command not allowed")
		return
	}
	c.activeHost, c.activePort, c.mode = address, port, dataActive
	c.reply("200", "Port command successful")
}

// cmdEprt handles EPRT.
func cmdEprt(c *conn, arg string) {
	if c.epsvAll {
		c.reply("501", "EPSV ALL in effect")
		return
	}
	c.mode = dataNone
	c.closeData()

	parts := strings.Split(arg, "|")
	if len(parts) != 5 {
		c.reply("501", "Extended port command failed")
		return
	}
	address := parts[2]
	port, err := strconv.Atoi(parts[3])
	if err != nil || net.ParseIP(address) == nil {
		c.reply("501", "Extended port command failed")
		return
	}
	if !c.isDataTargetAllowed(address, port) {
		c.reply("501", "Extended port command not allowed")
		return
	}
	c.activeHost, c.activePort, c.mode = address, port, dataActive
	c.reply("200", "Extended Port command successful")
}

// cmdPasv handles PASV.
func cmdPasv(c *conn, _ string) {
	if c.epsvAll {
		c.reply("501", "EPSV ALL in effect")
		return
	}
	// PASV can only name an IPv4 address, an IPv6 client has to use EPSV
	if net.ParseIP(c.localAddr) == nil || net.ParseIP(c.localAddr).To4() == nil {
		c.reply("522", "Network protocol not supported, use (2)")
		return
	}
	port, err := c.listenPassive()
	if err != nil {
		c.reply("501", "Passive command failed")
		return
	}
	c.log.Debug("ftp listening for a data connection", "port", port)
	octets := strings.ReplaceAll(c.localAddr, ".", ",")
	c.reply("227", fmt.Sprintf("Entering passive mode (%s,%d,%d)", octets, port/256, port%256))
}

// cmdEpsv handles EPSV.
func cmdEpsv(c *conn, arg string) {
	parameter := strings.ToUpper(strings.TrimSpace(arg))
	// EPSV ALL promises the client will only use EPSV from now on, so every
	// other data setup command has to be refused (RFC 2428).
	if parameter == "ALL" {
		c.epsvAll = true
		c.reply("200", "EPSV ALL command successful")
		return
	}
	if parameter != "" && parameter != "1" && parameter != "2" {
		c.reply("522", "Network protocol not supported, use (1,2)")
		return
	}
	port, err := c.listenPassive()
	if err != nil {
		c.reply("501", "Extended passive command failed")
		return
	}
	c.log.Debug("ftp listening for a data connection", "port", port)
	c.reply("229", fmt.Sprintf("Entering extended passive mode (|||%d|)", port))
}
