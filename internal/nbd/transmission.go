//go:build !light && !no_nbd

package nbd

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/sparse"
)

// request is one request of the transmission phase.
type request struct {
	flags  uint16
	kind   uint16
	cookie uint64
	offset uint64
	length uint32
	// data is the payload of a write, from the buffer pool
	data []byte
	// budgeted is what the request holds of the connection's budget
	budgeted int64
	// failed is the error the request was answered with, 0 for none, and
	// reason why, for the trace
	failed uint32
	reason string
}

// transmit runs the transmission phase: requests are read in order and run
// by up to maxInFlight workers, whose replies go out as they finish. It ends
// with NBD_CMD_DISC, a broken connection or a close from outside, and lets
// every request in flight finish before the image is synced and let go of.
func (c *conn) transmit() {
	exp := c.export
	started := time.Now()
	w := bufferedWriter(c.rw)
	var writeMu sync.Mutex
	var workers sync.WaitGroup
	slots := make(chan struct{}, maxInFlight)
	money := newBudget(inFlightBytes)
	var firstErr sync.Once

	t := &transmission{c: c, exp: exp, w: w, writeMu: &writeMu, ioError: func(op string, err error) {
		firstErr.Do(func() { c.log.Error("nbd i/o failed", "op", op, "error", err) })
	}}

	var ended error
requests:
	for {
		var header [28]byte
		if _, err := io.ReadFull(c.rw, header[:]); err != nil {
			ended = err
			break
		}
		if binary.BigEndian.Uint32(header[0:]) != requestMagic {
			ended = errors.New("a request does not start with the request magic")
			break
		}
		req := &request{
			flags:  binary.BigEndian.Uint16(header[4:]),
			kind:   binary.BigEndian.Uint16(header[6:]),
			cookie: binary.BigEndian.Uint64(header[8:]),
			offset: binary.BigEndian.Uint64(header[16:]),
			length: binary.BigEndian.Uint32(header[24:]),
		}
		c.traceRequest(req)
		switch req.kind {
		case cmdDisc:
			break requests
		case cmdWrite:
			// a payload this large is not one the server said it takes, and
			// not one it reads into memory to refuse
			if req.length > maxBlock {
				ended = errors.New("a write is larger than the largest block the server takes")
				break requests
			}
			req.budgeted = int64(req.length)
			if !money.acquire(req.budgeted) {
				ended = errors.New("the connection is closing")
				break requests
			}
			req.data = getBuffer(int(req.length))
			if _, err := io.ReadFull(c.rw, req.data); err != nil {
				money.release(req.budgeted)
				putBuffer(req.data)
				ended = err
				break requests
			}
		case cmdRead:
			// an oversized read is refused by its worker; it holds nothing
			if req.length <= maxBlock {
				req.budgeted = int64(req.length)
				if !money.acquire(req.budgeted) {
					ended = errors.New("the connection is closing")
					break requests
				}
			}
		}
		slots <- struct{}{}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots }()
			defer money.release(req.budgeted)
			t.handle(req)
		}()
	}
	money.close()
	workers.Wait()

	syncErr := error(nil)
	if exp.writable {
		syncErr = exp.img.Sync()
	}
	_ = exp.img.Close()
	exp.lease.Release()

	fields := []any{"read", exp.session.Read(), "written", exp.session.Written(),
		"took", time.Since(started).Round(time.Millisecond)}
	if reason := c.closedBecause(); reason != "" {
		fields = append(fields, "reason", reason)
	} else if ended != nil && !isGone(ended) {
		fields = append(fields, "error", ended)
	}
	if syncErr != nil {
		c.log.Error("nbd cannot sync the disk as it is detached", "error", syncErr)
	}
	c.log.Info("nbd detach", fields...)
}

// transmission is what the workers of one connection share.
type transmission struct {
	c   *conn
	exp *export
	w   interface {
		Write([]byte) (int, error)
		Flush() error
	}
	writeMu *sync.Mutex
	ioError func(op string, err error)
}

// handle runs one request and sends its reply.
func (t *transmission) handle(req *request) {
	if req.data != nil {
		defer putBuffer(req.data)
	}
	defer t.c.traceAnswer(req, time.Now())
	exp := t.exp
	if req.flags&^cmdFlagsKnown != 0 {
		t.fail(req, errInvalid, "the request has unknown flags")
		return
	}
	offset, length := int64(req.offset), int64(req.length)
	inside := req.offset <= uint64(exp.size) && uint64(length) <= uint64(exp.size)-req.offset

	switch req.kind {
	case cmdRead:
		switch {
		case req.length > maxBlock:
			t.fail(req, errInvalid, "the read is larger than the largest block the server takes")
		case !inside:
			t.fail(req, errInvalid, "the read reaches beyond the end of the disk")
		default:
			buf := getBuffer(int(req.length))
			defer putBuffer(buf)
			if _, err := exp.img.ReadAt(buf, offset); err != nil {
				t.ioError("read", err)
				t.fail(req, errno(err), "the disk cannot be read")
				return
			}
			exp.session.AddRead(length)
			t.data(req, buf)
		}

	case cmdWrite:
		switch {
		case !exp.writable:
			t.fail(req, errPerm, "the disk is attached read-only")
		case !inside:
			t.fail(req, errNoSpace, "the write reaches beyond the end of the disk")
		default:
			if _, err := exp.img.WriteAt(req.data, offset); err != nil {
				t.writeFailed(req, "write", err)
				return
			}
			exp.session.AddWritten(length)
			t.done(req, t.fua(req))
		}

	case cmdFlush:
		if exp.writable {
			if err := exp.img.Sync(); err != nil {
				t.writeFailed(req, "flush", err)
				return
			}
		}
		t.done(req, nil)

	case cmdTrim:
		switch {
		case !exp.writable:
			t.fail(req, errPerm, "the disk is attached read-only")
		case !inside:
			t.fail(req, errInvalid, "the trim reaches beyond the end of the disk")
		default:
			// a trim is advice: where no hole can be punched, the data stays
			if err := exp.img.PunchHole(offset, length); err != nil && !errors.Is(err, sparse.ErrUnsupported) {
				t.writeFailed(req, "trim", err)
				return
			}
			t.done(req, t.fua(req))
		}

	case cmdWriteZeroes:
		switch {
		case !exp.writable:
			t.fail(req, errPerm, "the disk is attached read-only")
		case !inside:
			t.fail(req, errNoSpace, "the write reaches beyond the end of the disk")
		default:
			if err := t.zero(req, offset, length); err != nil {
				return
			}
			t.done(req, t.fua(req))
		}

	case cmdCache:
		if !inside {
			t.fail(req, errInvalid, "the range reaches beyond the end of the disk")
			return
		}
		t.done(req, nil)

	case cmdBlockStatus:
		switch {
		case !t.c.structured || !t.c.metaBase:
			t.fail(req, errInvalid, "no metadata context was selected")
		case length == 0 || !inside:
			t.fail(req, errInvalid, "the range is empty or reaches beyond the end of the disk")
		default:
			extents, err := exp.img.Extents(offset, length)
			if err != nil {
				t.ioError("block status", err)
				t.fail(req, errno(err), "the extents cannot be read")
				return
			}
			t.blockStatus(req, extents)
		}

	default:
		t.fail(req, errInvalid, "the server does not know the command")
	}
}

// zero makes a range read as zeros: by punching a hole, unless the client
// wants it allocated, and failing at once where that would mean writing the
// zeros when the client asked for a fast zero. It answers a failure itself.
func (t *transmission) zero(req *request, offset, length int64) error {
	img := t.exp.img
	noHole := req.flags&cmdFlagNoHole != 0
	fast := req.flags&cmdFlagFastZero != 0
	if fast && (noHole || !sparse.HolePunching) {
		t.fail(req, errNotSup, "zeros cannot be written faster than by writing them")
		return errors.ErrUnsupported
	}
	var err error
	if !noHole {
		err = img.PunchHole(offset, length)
		if errors.Is(err, sparse.ErrUnsupported) {
			if fast {
				t.fail(req, errNotSup, "zeros cannot be written faster than by writing them")
				return err
			}
			err = img.ZeroRange(offset, length)
		}
	} else {
		err = img.ZeroRange(offset, length)
	}
	if err != nil {
		t.writeFailed(req, "write zeroes", err)
		return err
	}
	return nil
}

// fua syncs after a write that asked to be on stable storage before its
// reply, returning the error the reply carries.
func (t *transmission) fua(req *request) error {
	if req.flags&cmdFlagFUA == 0 {
		return nil
	}
	return t.exp.img.Sync()
}

// writeFailed answers a write that failed. A full filesystem is not logged
// as a failure of go-fs: QEMU pauses the VM until there is space again.
func (t *transmission) writeFailed(req *request, op string, err error) {
	if code := errno(err); code == errNoSpace {
		t.c.log.Warn("nbd disk full, the client is told ENOSPC", "op", op)
		t.fail(req, code, "there is no space left on the filesystem of the disk")
		return
	}
	t.ioError(op, err)
	t.fail(req, errno(err), "the disk cannot be written")
}

// done answers a request that carries nothing back, with the error err maps
// to, if any.
func (t *transmission) done(req *request, err error) {
	if err != nil {
		t.writeFailed(req, "sync", err)
		return
	}
	t.simple(req.cookie, 0, nil)
}

// fail answers a request with an error. A read or a block status asked for
// with structured replies has to fail with a structured reply.
func (t *transmission) fail(req *request, code uint32, message string) {
	req.failed, req.reason = code, message
	if t.c.structured && (req.kind == cmdRead || req.kind == cmdBlockStatus) {
		payload := make([]byte, 6+len(message))
		binary.BigEndian.PutUint32(payload[0:], code)
		binary.BigEndian.PutUint16(payload[4:], uint16(len(message)))
		copy(payload[6:], message)
		t.chunk(req.cookie, replyFlagDone, replyTypeError, payload, nil)
		return
	}
	t.simple(req.cookie, code, nil)
}

// data answers a read.
func (t *transmission) data(req *request, buf []byte) {
	if !t.c.structured {
		t.simple(req.cookie, 0, buf)
		return
	}
	if len(buf) == 0 {
		t.chunk(req.cookie, replyFlagDone, replyTypeNone, nil, nil)
		return
	}
	var offset [8]byte
	binary.BigEndian.PutUint64(offset[:], req.offset)
	t.chunk(req.cookie, replyFlagDone, replyTypeOffsetData, offset[:], buf)
}

// blockStatus answers a block status request with the extents of
// base:allocation.
func (t *transmission) blockStatus(req *request, extents []sparse.Extent) {
	if req.flags&cmdFlagReqOne != 0 && len(extents) > 1 {
		extents = extents[:1]
	}
	if len(extents) > maxExtents {
		extents = extents[:maxExtents]
	}
	payload := make([]byte, 4+8*len(extents))
	binary.BigEndian.PutUint32(payload[0:], contextBaseID)
	for i, extent := range extents {
		var state uint32
		if extent.Hole {
			state = stateHole | stateZero
		}
		binary.BigEndian.PutUint32(payload[4+8*i:], uint32(extent.Length))
		binary.BigEndian.PutUint32(payload[8+8*i:], state)
	}
	t.chunk(req.cookie, replyFlagDone, replyTypeBlockStatus, payload, nil)
}

// simple sends a simple reply.
func (t *transmission) simple(cookie uint64, code uint32, data []byte) {
	var header [16]byte
	binary.BigEndian.PutUint32(header[0:], simpleMagic)
	binary.BigEndian.PutUint32(header[4:], code)
	binary.BigEndian.PutUint64(header[8:], cookie)
	t.send(header[:], data)
}

// chunk sends one chunk of a structured reply: its payload is head and then
// data, which keeps the data of a read from being copied.
func (t *transmission) chunk(cookie uint64, flags, kind uint16, head, data []byte) {
	header := make([]byte, 20, 20+len(head))
	binary.BigEndian.PutUint32(header[0:], structuredMagic)
	binary.BigEndian.PutUint16(header[4:], flags)
	binary.BigEndian.PutUint16(header[6:], kind)
	binary.BigEndian.PutUint64(header[8:], cookie)
	binary.BigEndian.PutUint32(header[16:], uint32(len(head)+len(data)))
	t.send(append(header, head...), data)
}

// send writes one reply whole. A reply that cannot be sent ends the
// connection: the client would wait for it forever.
func (t *transmission) send(header, data []byte) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.w.Write(header); err == nil {
		if len(data) > 0 {
			_, err = t.w.Write(data)
		}
		if err == nil {
			err = t.w.Flush()
		}
		if err == nil {
			return
		}
	}
	t.c.close("")
}
