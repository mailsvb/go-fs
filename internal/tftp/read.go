package tftp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"time"
)

// readTransfer serves one read request. Data is sent in windows of windowSize
// blocks (RFC 7440); with the default window of one this is the lock step
// exchange of RFC 1350.
type readTransfer struct {
	server *Server
	// set is the snapshot this transfer started under, so a reload halfway
	// through does not change the rules it runs by.
	set  *settings
	slot admission
	req  request
	peer net.Addr
	conn *net.UDPConn
	log  *slog.Logger

	file io.Closer
	src  io.Reader

	blockSize  int
	windowSize int
	timeout    time.Duration
	retries    int
	acked      []option

	// windowBase is the number of blocks that have been acknowledged. It grows
	// past 65535, the block numbers on the wire are the low 16 bits of it.
	windowBase uint64
	chunks     [][]byte
	// sentCount is how many of chunks have already been transmitted.
	sentCount       int
	lastChunkLength int
	blocksSent      int
	bytesSent       int64
	// newBlocks and resentBlocks budget the window rewind, see ack.
	newBlocks     int
	resentBlocks  int
	pendingResend bool

	streamEnded bool
}

func (s *Server) startRead(ctx context.Context, set *settings, slot admission, req request, from net.Addr, file *os.File, size int64) {
	conn, err := s.transferSocket()
	if err != nil {
		s.log.Error("tftp cannot open a transfer socket", "error", err)
		_ = file.Close()
		s.release(slot)
		s.sendTo(from, encodeError(errNotDefined, "Internal error"))
		return
	}

	opts := negotiate(req, set.limits, true, size)
	t := &readTransfer{
		server:     s,
		set:        set,
		slot:       slot,
		req:        req,
		peer:       from,
		conn:       conn,
		log:        s.log.With("client", slot.client, "file", sanitize(req.filename)),
		file:       file,
		blockSize:  opts.blockSize,
		windowSize: opts.windowSize,
		timeout:    opts.timeout,
		retries:    set.limits.retries,
		acked:      opts.acked,
	}
	if req.mode == modeNetascii {
		t.src = newNetasciiReader(file)
	} else {
		t.src = file
	}

	transferCtx, cancel := context.WithCancel(ctx)
	s.begin(slot, cancel)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		t.run(transferCtx)
	}()
}

func (t *readTransfer) run(ctx context.Context) {
	defer t.cleanup()

	hardDeadline := noDeadline
	if t.set.cfg.TransferTimeout > 0 {
		hardDeadline = time.Now().Add(time.Duration(t.set.cfg.TransferTimeout) * time.Second)
	}
	go func() {
		<-ctx.Done()
		_ = t.conn.SetReadDeadline(time.Now())
	}()

	if len(t.acked) > 0 {
		if !t.handshake(ctx, hardDeadline) {
			return
		}
	}
	if !t.fillAndSend() {
		return
	}

	for {
		msg, ok := t.await(ctx, hardDeadline, func() { t.sendWindow(true) })
		if !ok {
			return
		}
		if len(msg) < 4 {
			continue
		}
		op := opcode(uint16(msg[0])<<8 | uint16(msg[1]))
		if op == opERROR {
			t.log.Debug("tftp client aborted the transfer")
			return
		}
		if op != opACK {
			t.send(encodeError(errIllegalOperation, "Illegal TFTP operation"))
			return
		}
		if done := t.ack(uint16(msg[2])<<8 | uint16(msg[3])); done {
			return
		}
	}
}

// handshake sends the OACK and waits for the client to acknowledge block zero.
func (t *readTransfer) handshake(ctx context.Context, hardDeadline time.Time) bool {
	t.send(encodeOACK(t.acked))
	for {
		msg, ok := t.await(ctx, hardDeadline, func() { t.send(encodeOACK(t.acked)) })
		if !ok {
			return false
		}
		if len(msg) < 4 {
			continue
		}
		op := opcode(uint16(msg[0])<<8 | uint16(msg[1]))
		if op == opERROR {
			t.log.Debug("tftp client aborted the transfer")
			return false
		}
		if op != opACK {
			t.send(encodeError(errIllegalOperation, "Illegal TFTP operation"))
			return false
		}
		if block := uint16(msg[2])<<8 | uint16(msg[3]); block == 0 {
			return true
		}
	}
}

// ack processes an acknowledgement and reports whether the transfer is over.
func (t *readTransfer) ack(block uint16) bool {
	acked := int(block - uint16(t.windowBase))
	if acked == 0 || acked > len(t.chunks) {
		return false // stale or invalid, the client will be resent to on timeout
	}

	// the transfer ends with a block shorter than the block size
	if len(t.chunks[acked-1]) < t.blockSize {
		t.complete()
		return true
	}

	// A partial acknowledgement means the receiver missed something, so the
	// rest of the window goes out again (RFC 7440). A receiver that
	// acknowledges every single block instead of once per window would make
	// that happen on every packet, so rewinding is budgeted against the blocks
	// that actually moved forward. Normal loss stays well inside that budget, a
	// misbehaving receiver falls back to plain sliding after two windows.
	t.pendingResend = acked < len(t.chunks) && t.resentBlocks <= t.newBlocks

	t.windowBase += uint64(acked)
	t.chunks = t.chunks[acked:]
	t.sentCount = max(0, t.sentCount-acked)
	return !t.fillAndSend()
}

// fillAndSend tops the window up from the file and transmits it. It reports
// false once the transfer is finished or failed.
func (t *readTransfer) fillAndSend() bool {
	for len(t.chunks) < t.windowSize && !t.streamEnded {
		chunk, err := t.readChunk()
		if err != nil {
			t.log.Debug("tftp read error", "error", err)
			t.send(encodeError(errNotDefined, "Read error"))
			return false
		}
		if chunk == nil {
			t.streamEnded = true
			break
		}
		t.chunks = append(t.chunks, chunk)
	}

	if len(t.chunks) > 0 {
		resend := t.pendingResend
		t.pendingResend = false
		t.sendWindow(resend)
		return true
	}

	// A transfer is terminated by a block shorter than the block size. An empty
	// file, or a file whose size is a multiple of the block size, needs a final
	// empty block of its own.
	if t.blocksSent == 0 || t.lastChunkLength == t.blockSize {
		t.chunks = append(t.chunks, []byte{})
		t.sendWindow(false)
		return true
	}

	t.complete()
	return false
}

// readChunk returns the next block, or nil at the end of the file.
func (t *readTransfer) readChunk() ([]byte, error) {
	buf := make([]byte, t.blockSize)
	n, err := io.ReadFull(t.src, buf)
	if n > 0 {
		return buf[:n], nil
	}
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	return nil, err
}

// sendWindow transmits the window. Blocks already on the wire are only repeated
// when the receiver asked for them again, either through a partial
// acknowledgement or by staying silent until the retransmit timer fired.
func (t *readTransfer) sendWindow(resend bool) {
	alreadySent := t.sentCount
	from := alreadySent
	if resend {
		from = 0
	}
	for i := from; i < len(t.chunks); i++ {
		block := uint16(t.windowBase + 1 + uint64(i))
		t.send(encodeDATA(block, t.chunks[i]))
		t.blocksSent++
		t.bytesSent += int64(len(t.chunks[i]))
		if i < alreadySent {
			t.resentBlocks++
		} else {
			t.newBlocks++
		}
	}
	t.sentCount = len(t.chunks)
	t.lastChunkLength = len(t.chunks[len(t.chunks)-1])
}

func (t *readTransfer) complete() {
	t.log.Info("tftp transfer complete",
		"direction", "download",
		"bytes", t.bytesSent,
		"blksize", t.blockSize,
		"windowsize", t.windowSize,
		"mode", t.req.mode)
}

func (t *readTransfer) send(packet []byte) {
	if _, err := t.conn.WriteTo(packet, t.peer); err != nil {
		t.log.Debug("tftp send failed", "error", err)
	}
}

func (t *readTransfer) cleanup() {
	_ = t.file.Close()
	_ = t.conn.Close()
	t.server.release(t.slot)
}

// await reads the next packet from the client. On every timeout it calls resend
// and tries again, giving up after the configured number of retries or when the
// transfer outlives its deadline. Packets from a different address are answered
// with an unknown transfer id and do not count as an attempt.
func (t *readTransfer) await(ctx context.Context, hardDeadline time.Time, resend func()) ([]byte, bool) {
	return awaitPacket(ctx, t.conn, t.peer, t.log, t.timeout, t.retries, hardDeadline, resend)
}

var noDeadline = time.Time{}

func awaitPacket(ctx context.Context, conn *net.UDPConn, peer net.Addr, log *slog.Logger,
	timeout time.Duration, retries int, hardDeadline time.Time, resend func()) ([]byte, bool) {

	buf := make([]byte, 65536)
	for attempt := 0; attempt <= retries; attempt++ {
		for {
			deadline := time.Now().Add(timeout)
			if !hardDeadline.IsZero() && hardDeadline.Before(deadline) {
				deadline = hardDeadline
			}
			_ = conn.SetReadDeadline(deadline)

			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					if ctx.Err() != nil {
						return nil, false
					}
					if !hardDeadline.IsZero() && !time.Now().Before(hardDeadline) {
						log.Debug("tftp transfer exceeded its deadline, aborting")
						_, _ = conn.WriteTo(encodeError(errNotDefined, "Transfer took too long"), peer)
						return nil, false
					}
					break // retransmit and start another attempt
				}
				return nil, false
			}
			if !sameClient(from, peer) {
				_, _ = conn.WriteTo(encodeError(errUnknownTID, "Unknown transfer ID"), from)
				continue
			}
			msg := make([]byte, n)
			copy(msg, buf[:n])
			return msg, true
		}
		if attempt < retries {
			resend()
		}
	}
	log.Debug("tftp transfer timed out", "retries", retries)
	return nil, false
}
