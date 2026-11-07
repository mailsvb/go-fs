//go:build !light && !no_nbd

package nbd

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// client is the NBD client of the tests: just enough of the protocol to
// drive the server through every path, and nothing that hides a mistake.
type client struct {
	t          *testing.T
	raw        net.Conn
	rw         net.Conn
	structured bool
	// reads are the lengths of the reads in flight, by cookie, which a
	// simple reply does not repeat
	reads map[uint64]uint32
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	c := &client{t: t, raw: raw, rw: raw, reads: map[uint64]uint32{}}
	var greeting [18]byte
	if _, err := io.ReadFull(raw, greeting[:]); err != nil {
		t.Fatalf("no greeting: %v", err)
	}
	if binary.BigEndian.Uint64(greeting[0:]) != nbdMagic || binary.BigEndian.Uint64(greeting[8:]) != optMagic {
		t.Fatalf("the greeting is %x", greeting)
	}
	if flags := binary.BigEndian.Uint16(greeting[16:]); flags != flagFixedNewstyle|flagNoZeroes {
		t.Fatalf("the handshake flags are %#x", flags)
	}
	if err := binary.Write(raw, binary.BigEndian, clientFlagFixedNewstyle|clientFlagNoZeroes); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *client) option(option uint32, data []byte) {
	c.t.Helper()
	b := make([]byte, 16+len(data))
	binary.BigEndian.PutUint64(b[0:], optMagic)
	binary.BigEndian.PutUint32(b[8:], option)
	binary.BigEndian.PutUint32(b[12:], uint32(len(data)))
	copy(b[16:], data)
	if _, err := c.rw.Write(b); err != nil {
		c.t.Fatalf("option %d: %v", option, err)
	}
}

type optReply struct {
	option, kind uint32
	data         []byte
}

func (c *client) readReply() (optReply, error) {
	var header [20]byte
	if _, err := io.ReadFull(c.rw, header[:]); err != nil {
		return optReply{}, err
	}
	if binary.BigEndian.Uint64(header[0:]) != replyMagic {
		return optReply{}, fmt.Errorf("the reply magic is %x", header[:8])
	}
	r := optReply{option: binary.BigEndian.Uint32(header[8:]), kind: binary.BigEndian.Uint32(header[12:])}
	r.data = make([]byte, binary.BigEndian.Uint32(header[16:]))
	_, err := io.ReadFull(c.rw, r.data)
	return r, err
}

func (c *client) mustReply(option, kind uint32) optReply {
	c.t.Helper()
	r, err := c.readReply()
	if err != nil {
		c.t.Fatalf("waiting for the reply to option %d: %v", option, err)
	}
	if r.option != option || r.kind != kind {
		c.t.Fatalf("option %d got reply %#x %q, want %#x", option, r.kind, r.data, kind)
	}
	return r
}

// startTLS switches to TLS with cert, and reports the error of the first
// round trip after the handshake, which is where TLS 1.3 tells a client its
// certificate was refused.
func (c *client) startTLS(ca *x509.CertPool, cert tls.Certificate) error {
	c.t.Helper()
	c.option(optStartTLS, nil)
	c.mustReply(optStartTLS, repAck)
	tc := tls.Client(c.raw, &tls.Config{RootCAs: ca, ServerName: "127.0.0.1", Certificates: []tls.Certificate{cert}})
	if err := tc.Handshake(); err != nil {
		return err
	}
	c.rw = tc
	c.option(optStructuredReply, []byte("x")) // malformed on purpose: a cheap round trip
	if _, err := c.readReply(); err != nil {
		return err
	}
	return nil
}

func (c *client) structuredReplies() {
	c.t.Helper()
	c.option(optStructuredReply, nil)
	c.mustReply(optStructuredReply, repAck)
	c.structured = true
}

func (c *client) setMeta(name string, queries ...string) []optReply {
	c.t.Helper()
	c.option(optSetMetaContext, metaRequest(name, queries...))
	var replies []optReply
	for {
		r, err := c.readReply()
		if err != nil {
			c.t.Fatal(err)
		}
		if r.kind == repAck {
			return replies
		}
		replies = append(replies, r)
	}
}

func metaRequest(name string, queries ...string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(name)))
	b = append(b, name...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(queries)))
	for _, q := range queries {
		b = binary.BigEndian.AppendUint32(b, uint32(len(q)))
		b = append(b, q...)
	}
	return b
}

func goRequest(name string, infos ...uint16) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(name)))
	b = append(b, name...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(infos)))
	for _, info := range infos {
		b = binary.BigEndian.AppendUint16(b, info)
	}
	return b
}

type exportInfo struct {
	size                     uint64
	flags                    uint16
	minimum, preferred, most uint32
}

// goExport attaches name, or returns the refusal.
func (c *client) goExport(option uint32, name string) (exportInfo, *optReply) {
	c.t.Helper()
	c.option(option, goRequest(name, infoBlockSize))
	var info exportInfo
	for {
		r, err := c.readReply()
		if err != nil {
			c.t.Fatalf("option %d of %q: %v", option, name, err)
		}
		switch {
		case r.kind == repAck:
			return info, nil
		case r.kind&repFlagError != 0:
			return info, &r
		case r.kind == repInfo && binary.BigEndian.Uint16(r.data) == infoExport:
			info.size = binary.BigEndian.Uint64(r.data[2:])
			info.flags = binary.BigEndian.Uint16(r.data[10:])
		case r.kind == repInfo && binary.BigEndian.Uint16(r.data) == infoBlockSize:
			info.minimum = binary.BigEndian.Uint32(r.data[2:])
			info.preferred = binary.BigEndian.Uint32(r.data[6:])
			info.most = binary.BigEndian.Uint32(r.data[10:])
		}
	}
}

func (c *client) send(kind, flags uint16, cookie, offset uint64, length uint32, data []byte) {
	c.t.Helper()
	b := make([]byte, 28+len(data))
	binary.BigEndian.PutUint32(b[0:], requestMagic)
	binary.BigEndian.PutUint16(b[4:], flags)
	binary.BigEndian.PutUint16(b[6:], kind)
	binary.BigEndian.PutUint64(b[8:], cookie)
	binary.BigEndian.PutUint64(b[16:], offset)
	binary.BigEndian.PutUint32(b[24:], length)
	copy(b[28:], data)
	if kind == cmdRead {
		c.reads[cookie] = length
	}
	if _, err := c.rw.Write(b); err != nil {
		c.t.Fatalf("request %d: %v", kind, err)
	}
}

// reply is a reply of the transmission phase, simple or the chunks of a
// structured one gathered up.
type reply struct {
	cookie  uint64
	errCode uint32
	message string
	data    []byte
	// extents are those of a block status, length and state
	extents [][2]uint32
	// kinds are the types of the chunks, for a structured reply
	kinds []uint16
}

func (c *client) receive() (reply, error) {
	var magic [4]byte
	if _, err := io.ReadFull(c.rw, magic[:]); err != nil {
		return reply{}, err
	}
	switch binary.BigEndian.Uint32(magic[:]) {
	case simpleMagic:
		var rest [12]byte
		if _, err := io.ReadFull(c.rw, rest[:]); err != nil {
			return reply{}, err
		}
		r := reply{errCode: binary.BigEndian.Uint32(rest[0:]), cookie: binary.BigEndian.Uint64(rest[4:])}
		if length, isRead := c.reads[r.cookie]; isRead {
			delete(c.reads, r.cookie)
			if r.errCode == 0 {
				r.data = make([]byte, length)
				if _, err := io.ReadFull(c.rw, r.data); err != nil {
					return r, err
				}
			}
		}
		return r, nil
	case structuredMagic:
		var r reply
		for first := true; ; first = false {
			if !first {
				if _, err := io.ReadFull(c.rw, magic[:]); err != nil {
					return r, err
				}
				if binary.BigEndian.Uint32(magic[:]) != structuredMagic {
					return r, errors.New("a chunk does not start with the structured magic")
				}
			}
			var rest [16]byte
			if _, err := io.ReadFull(c.rw, rest[:]); err != nil {
				return r, err
			}
			flags := binary.BigEndian.Uint16(rest[0:])
			kind := binary.BigEndian.Uint16(rest[2:])
			r.cookie = binary.BigEndian.Uint64(rest[4:])
			payload := make([]byte, binary.BigEndian.Uint32(rest[12:]))
			if _, err := io.ReadFull(c.rw, payload); err != nil {
				return r, err
			}
			r.kinds = append(r.kinds, kind)
			switch kind {
			case replyTypeOffsetData:
				r.data = append(r.data, payload[8:]...)
			case replyTypeBlockStatus:
				for p := payload[4:]; len(p) >= 8; p = p[8:] {
					r.extents = append(r.extents, [2]uint32{binary.BigEndian.Uint32(p), binary.BigEndian.Uint32(p[4:])})
				}
			case replyTypeError, replyTypeErrorOffset:
				r.errCode = binary.BigEndian.Uint32(payload)
				n := binary.BigEndian.Uint16(payload[4:])
				r.message = string(payload[6 : 6+n])
			}
			if flags&replyFlagDone != 0 {
				delete(c.reads, r.cookie)
				return r, nil
			}
		}
	default:
		return reply{}, fmt.Errorf("the reply magic is %x", magic)
	}
}

// do sends one request and waits for its reply.
func (c *client) do(kind, flags uint16, offset uint64, length uint32, data []byte) reply {
	c.t.Helper()
	cookie := uint64(time.Now().UnixNano())
	c.send(kind, flags, cookie, offset, length, data)
	r, err := c.receive()
	if err != nil {
		c.t.Fatalf("request %d: %v", kind, err)
	}
	if r.cookie != cookie {
		c.t.Fatalf("request %d got the reply of cookie %d", kind, r.cookie)
	}
	return r
}

// closed reports whether the server has closed the connection, waiting up
// to a few seconds for it.
func (c *client) closed() bool {
	_ = c.rw.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.rw.SetReadDeadline(time.Time{}) }()
	var b [1]byte
	_, err := c.rw.Read(b[:])
	var netErr net.Error
	return err != nil && !(errors.As(err, &netErr) && netErr.Timeout())
}

func dialRaw(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 5*time.Second)
}
