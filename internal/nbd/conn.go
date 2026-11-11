//go:build !light && !no_nbd

package nbd

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailsvb/go-fs/internal/disks"
	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/sparse"
)

// conn is one client connection.
type conn struct {
	srv    *Server
	raw    net.Conn
	rw     net.Conn // raw, or the TLS connection on top of it
	remote string
	log    *slog.Logger

	closeOnce sync.Once
	reason    atomic.Pointer[string]

	idMu sync.Mutex
	id   identity

	// what the options negotiated
	structured bool
	noZeroes   bool
	metaExport string
	metaBase   bool

	// the attached image, once NBD_OPT_GO or NBD_OPT_EXPORT_NAME chose one
	export *export
}

// export is the image a connection has attached.
type export struct {
	name     string
	size     int64
	writable bool
	img      image
	lease    *disks.Lease
	session  *disks.Session
}

func newConn(s *Server, raw net.Conn) *conn {
	remote := raw.RemoteAddr().String()
	return &conn{srv: s, raw: raw, rw: raw, remote: remote, log: s.log.With("client", remote)}
}

func (c *conn) identity() identity {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	return c.id
}

// close ends the connection, from any goroutine, saying why. Closing the
// socket is what wakes the goroutine that reads requests, which then lets the
// requests in flight finish and lets go of the image.
func (c *conn) close(reason string) {
	c.closeOnce.Do(func() {
		c.reason.Store(&reason)
		_ = c.raw.Close()
	})
}

// closedBecause is why the connection was closed from outside, empty when it
// was not.
func (c *conn) closedBecause() string {
	if reason := c.reason.Load(); reason != nil {
		return *reason
	}
	return ""
}

// serve runs the connection from the greeting to its end.
func (c *conn) serve() {
	c.log.Debug("nbd connection established")
	defer c.close("")
	set := c.srv.settings()
	_ = c.raw.SetDeadline(time.Now().Add(time.Duration(set.cfg.HandshakeTimeout) * time.Second))

	ok, err := c.negotiate()
	if err != nil {
		if reason := c.closedBecause(); reason != "" {
			c.log.Debug("nbd connection closed during the handshake", "reason", reason)
		} else if !isGone(err) {
			c.log.Info("nbd handshake failed", "error", err)
		} else {
			c.log.Debug("nbd client left during the handshake", "error", err)
		}
		return
	}
	if !ok {
		c.log.Debug("nbd client ended the handshake")
		return
	}
	_ = c.raw.SetDeadline(time.Time{})
	c.transmit()
}

// negotiate runs the greeting and the options. It reports true once an image
// is attached and the transmission phase begins.
func (c *conn) negotiate() (bool, error) {
	greeting := make([]byte, 18)
	binary.BigEndian.PutUint64(greeting[0:], nbdMagic)
	binary.BigEndian.PutUint64(greeting[8:], optMagic)
	binary.BigEndian.PutUint16(greeting[16:], flagFixedNewstyle|flagNoZeroes)
	if _, err := c.raw.Write(greeting); err != nil {
		return false, err
	}
	var flags uint32
	if err := binary.Read(c.raw, binary.BigEndian, &flags); err != nil {
		return false, err
	}
	if flags&clientFlagFixedNewstyle == 0 {
		return false, errors.New("the client does not speak fixed newstyle negotiation")
	}
	if flags&^(clientFlagFixedNewstyle|clientFlagNoZeroes) != 0 {
		return false, fmt.Errorf("the client sent the unknown flags %#x", flags)
	}
	c.noZeroes = flags&clientFlagNoZeroes != 0
	c.log.Debug("nbd client flags", "flags", fmt.Sprintf("%#x", flags), "noZeroes", c.noZeroes)

	for {
		var header [16]byte
		if _, err := io.ReadFull(c.rw, header[:]); err != nil {
			return false, err
		}
		if binary.BigEndian.Uint64(header[0:]) != optMagic {
			return false, errors.New("an option does not start with IHAVEOPT")
		}
		option := binary.BigEndian.Uint32(header[8:])
		length := binary.BigEndian.Uint32(header[12:])
		if length > maxOption {
			return false, fmt.Errorf("option %d is %d bytes long", option, length)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(c.rw, data); err != nil {
			return false, err
		}
		c.log.Debug("nbd <", "option", optionName(option), "bytes", length)

		tlsDone := c.rw != c.raw
		switch {
		case option == optAbort:
			_ = c.reply(option, repAck, nil)
			return false, nil
		case option == optStartTLS:
			if err := c.startTLS(length); err != nil {
				return false, err
			}
		case !tlsDone && option == optExportName:
			// there is no way to refuse NBD_OPT_EXPORT_NAME but to hang up
			return false, errors.New("the client asked for an export before switching to TLS")
		case !tlsDone:
			if err := c.replyError(option, repErrTLSReqd, "switch to TLS with NBD_OPT_STARTTLS first"); err != nil {
				return false, err
			}
		case option == optExportName:
			return c.exportName(data)
		case option == optInfo || option == optGo:
			attached, err := c.infoOrGo(option, data)
			if err != nil || attached {
				return attached, err
			}
		case option == optList:
			if err := c.list(length); err != nil {
				return false, err
			}
		case option == optStructuredReply:
			if length != 0 {
				if err := c.replyError(option, repErrInvalid, "NBD_OPT_STRUCTURED_REPLY carries no data"); err != nil {
					return false, err
				}
				continue
			}
			c.structured = true
			if err := c.reply(option, repAck, nil); err != nil {
				return false, err
			}
		case option == optListMetaContext || option == optSetMetaContext:
			if err := c.metaContext(option, data); err != nil {
				return false, err
			}
		default:
			if err := c.replyError(option, repErrUnsup, "this server does not know the option"); err != nil {
				return false, err
			}
		}
	}
}

// startTLS switches the connection to TLS. The client certificate is
// checked by the handshake itself, against the settings of this moment.
func (c *conn) startTLS(length uint32) error {
	if c.rw != c.raw {
		return c.replyError(optStartTLS, repErrInvalid, "the connection is TLS already")
	}
	if length != 0 {
		return c.replyError(optStartTLS, repErrInvalid, "NBD_OPT_STARTTLS carries no data")
	}
	if err := c.reply(optStartTLS, repAck, nil); err != nil {
		return err
	}
	set := c.srv.settings()
	server := tls.Server(c.raw, set.tlsConfig(func(id identity) {
		c.idMu.Lock()
		c.id = id
		c.idMu.Unlock()
	}))
	if err := server.Handshake(); err != nil {
		if !isGone(err) {
			c.log.Info("nbd login refused", "error", err)
		}
		return errLoggedAlready
	}
	c.rw = server
	id := c.identity()
	c.log = c.log.With("user", id.account)
	state := server.ConnectionState()
	c.log.Debug("nbd login", "certificate", id.cert.SerialNumber.Text(16), "fingerprint", fingerprint(id.cert),
		"tls", logging.TLS(&state))
	return nil
}

// errLoggedAlready ends a connection whose failure was logged where it
// happened.
var errLoggedAlready = errors.New("logged already")

// reply sends an option reply.
func (c *conn) reply(option, kind uint32, data []byte) error {
	b := make([]byte, 20+len(data))
	binary.BigEndian.PutUint64(b[0:], replyMagic)
	binary.BigEndian.PutUint32(b[8:], option)
	binary.BigEndian.PutUint32(b[12:], kind)
	binary.BigEndian.PutUint32(b[16:], uint32(len(data)))
	copy(b[20:], data)
	c.log.Debug("nbd >", "option", optionName(option), "reply", replyName(kind), "bytes", len(data))
	_, err := c.rw.Write(b)
	return err
}

// replyError refuses an option, with a message for whoever reads the
// client's log.
func (c *conn) replyError(option, kind uint32, message string) error {
	c.log.Debug("nbd option refused", "option", option, "reason", message)
	return c.reply(option, kind, []byte(message))
}

// list names the images the account may attach, which is every one of them.
func (c *conn) list(length uint32) error {
	if length != 0 {
		return c.replyError(optList, repErrInvalid, "NBD_OPT_LIST carries no data")
	}
	images, err := c.srv.folder.List()
	if err != nil {
		c.log.Error("nbd cannot list the disks", "error", err)
		return c.replyError(optList, repErrPlatform, "the disks cannot be listed")
	}
	for _, image := range images {
		data := make([]byte, 4+len(image.Name))
		binary.BigEndian.PutUint32(data, uint32(len(image.Name)))
		copy(data[4:], image.Name)
		if err := c.reply(optList, repServer, data); err != nil {
			return err
		}
	}
	return c.reply(optList, repAck, nil)
}

// metaContext answers NBD_OPT_LIST_META_CONTEXT and selects contexts for
// NBD_OPT_SET_META_CONTEXT. base:allocation is the only one there is.
func (c *conn) metaContext(option uint32, data []byte) error {
	name, queries, ok := parseMetaContext(data)
	if !ok {
		return c.replyError(option, repErrInvalid, "the request is malformed")
	}
	if option == optSetMetaContext {
		if !c.structured {
			return c.replyError(option, repErrInvalid, "metadata contexts need structured replies")
		}
		c.metaBase = false
		c.metaExport = name
	}
	matches := false
	if len(queries) == 0 {
		// listing with no query lists everything; selecting nothing
		// selects nothing
		matches = option == optListMetaContext
	}
	for _, query := range queries {
		if query == contextBaseAllocation || (option == optListMetaContext && query == "base:") {
			matches = true
		}
	}
	if matches {
		data := make([]byte, 4+len(contextBaseAllocation))
		binary.BigEndian.PutUint32(data, contextBaseID)
		copy(data[4:], contextBaseAllocation)
		if err := c.reply(option, repMetaContext, data); err != nil {
			return err
		}
		if option == optSetMetaContext {
			c.metaBase = true
		}
	}
	return c.reply(option, repAck, nil)
}

func parseMetaContext(data []byte) (name string, queries []string, ok bool) {
	field := func() (string, bool) {
		if len(data) < 4 {
			return "", false
		}
		n := binary.BigEndian.Uint32(data)
		if uint64(n) > uint64(len(data)-4) {
			return "", false
		}
		value := string(data[4 : 4+n])
		data = data[4+n:]
		return value, true
	}
	if name, ok = field(); !ok || len(data) < 4 {
		return "", nil, false
	}
	count := binary.BigEndian.Uint32(data)
	data = data[4:]
	for range count {
		query, ok := field()
		if !ok {
			return "", nil, false
		}
		queries = append(queries, query)
	}
	return name, queries, len(data) == 0
}

// flags are the transmission flags of an image.
func (c *conn) flags(writable bool) uint16 {
	flags := tflagHasFlags | tflagSendFlush | tflagSendCache
	if writable {
		flags |= tflagSendFUA | tflagSendTrim | tflagWriteZeroes
		if sparse.HolePunching {
			flags |= tflagSendFastZero
		}
	} else {
		// many readers see the same bytes whichever connection they ask
		flags |= tflagReadOnly | tflagMultiConn
	}
	if c.structured {
		flags |= tflagSendDF
	}
	return flags
}

// infoOrGo answers NBD_OPT_INFO, and attaches the image for NBD_OPT_GO.
func (c *conn) infoOrGo(option uint32, data []byte) (bool, error) {
	if len(data) < 6 {
		return false, c.replyError(option, repErrInvalid, "the request is malformed")
	}
	length := binary.BigEndian.Uint32(data)
	if length > maxNameLength || uint64(length)+6 > uint64(len(data)) {
		return false, c.replyError(option, repErrInvalid, "the request is malformed")
	}
	name := string(data[4 : 4+length])
	rest := data[4+length:]
	count := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if len(rest) != 2*count {
		return false, c.replyError(option, repErrInvalid, "the request is malformed")
	}
	wantBlockSize, wantName := false, false
	for i := range count {
		switch binary.BigEndian.Uint16(rest[2*i:]) {
		case infoBlockSize:
			wantBlockSize = true
		case infoName:
			wantName = true
		}
	}

	id := c.identity()
	var size int64
	writable := id.writable
	if option == optGo {
		exp, kind, message := c.attach(name)
		if exp == nil {
			return false, c.replyError(option, kind, message)
		}
		c.export = exp
		size = exp.size
	} else {
		info, err := c.srv.folder.Stat(name)
		if err != nil {
			kind, message := refusal(name, err)
			return false, c.replyError(option, kind, message)
		}
		size = info.Size()
	}

	export := make([]byte, 12)
	binary.BigEndian.PutUint16(export[0:], infoExport)
	binary.BigEndian.PutUint64(export[2:], uint64(size))
	binary.BigEndian.PutUint16(export[10:], c.flags(writable))
	if err := c.reply(option, repInfo, export); err != nil {
		return false, err
	}
	if wantName {
		info := make([]byte, 2+len(name))
		binary.BigEndian.PutUint16(info, infoName)
		copy(info[2:], name)
		if err := c.reply(option, repInfo, info); err != nil {
			return false, err
		}
	}
	if wantBlockSize {
		info := make([]byte, 14)
		binary.BigEndian.PutUint16(info[0:], infoBlockSize)
		binary.BigEndian.PutUint32(info[2:], minBlock)
		binary.BigEndian.PutUint32(info[6:], preferredBlock)
		binary.BigEndian.PutUint32(info[10:], maxBlock)
		if err := c.reply(option, repInfo, info); err != nil {
			return false, err
		}
	}
	if err := c.reply(option, repAck, nil); err != nil {
		return false, err
	}
	return option == optGo, nil
}

// exportName attaches the image the old way, NBD_OPT_EXPORT_NAME, which has
// no way to refuse but to hang up.
func (c *conn) exportName(data []byte) (bool, error) {
	if len(data) > maxNameLength {
		return false, errors.New("the export name is too long")
	}
	exp, _, message := c.attach(string(data))
	if exp == nil {
		return false, errors.New(message)
	}
	c.export = exp
	b := make([]byte, 10, 134)
	binary.BigEndian.PutUint64(b[0:], uint64(exp.size))
	binary.BigEndian.PutUint16(b[8:], c.flags(exp.writable))
	if !c.noZeroes {
		b = b[:134]
	}
	if _, err := c.rw.Write(b); err != nil {
		return false, err
	}
	return true, nil
}

// attach attaches the image name for the account, read-write where it may
// write, or says how to refuse it.
func (c *conn) attach(name string) (*export, uint32, string) {
	id := c.identity()
	if name != c.metaExport {
		// contexts selected for another export do not carry over
		c.metaBase = false
	}
	session := disks.NewSession(c.close)
	session.Account = id.account
	session.Client = c.remote
	session.Writable = id.writable
	session.Fingerprint = fingerprint(id.cert)
	session.Serial = id.cert.SerialNumber.Text(16)
	session.Expires = id.cert.NotAfter
	file, lease, err := c.srv.folder.Attach(name, session)
	if err != nil {
		kind, message := refusal(name, err)
		if errors.Is(err, disks.ErrBusy) {
			c.log.Info("nbd attach refused", "disk", name, "reason", err)
		}
		return nil, kind, message
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		lease.Release()
		c.log.Error("nbd cannot read the disk", "disk", name, "error", err)
		return nil, repErrPlatform, "the disk cannot be read"
	}
	var img image = fileImage{File: file, log: c.log.With("disk", name)}
	if c.srv.wrap != nil {
		img = c.srv.wrap(img)
	}
	mode := "read-only"
	if id.writable {
		mode = "read-write"
	}
	c.log = c.log.With("disk", name)
	c.log.Info("nbd attach", "mode", mode, "size", info.Size())
	return &export{name: name, size: info.Size(), writable: id.writable, img: img, lease: lease, session: session}, 0, ""
}

// refusal is the option reply that refuses the image name for err.
func refusal(name string, err error) (uint32, string) {
	switch {
	case errors.Is(err, disks.ErrName), errors.Is(err, fs.ErrNotExist):
		return repErrUnknown, fmt.Sprintf("there is no disk %q", name)
	case errors.Is(err, disks.ErrBusy):
		return repErrPolicy, err.Error()
	default:
		return repErrPlatform, "the disk cannot be opened"
	}
}

// isGone reports an error that only says the other side went away.
func isGone(err error) bool {
	var netErr net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.As(err, &netErr)
}

// bufferedWriter is the writer replies go through, flushed once per reply.
func bufferedWriter(w io.Writer) *bufio.Writer { return bufio.NewWriterSize(w, 64<<10) }
