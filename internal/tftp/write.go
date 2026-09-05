package tftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"
)

// writeTransfer receives one write request. Write requests run lock step: every
// block is acknowledged before the next one is accepted, so the client is
// throttled by however fast the file system takes the data.
type writeTransfer struct {
	server *Server
	slot   admission
	req    request
	peer   net.Addr
	conn   *net.UDPConn
	log    *slog.Logger

	file *os.File
	sink io.Writer
	// netascii holds the converter when one is in use, so it can be flushed.
	netascii *netasciiWriter

	blockSize int
	timeout   time.Duration
	retries   int
	acked     []option

	expectedBlock uint32
	bytesWritten  int64
}

func (s *Server) startWrite(ctx context.Context, slot admission, req request, from net.Addr, file *os.File) {
	conn, err := s.transferSocket()
	if err != nil {
		s.log.Error("tftp cannot open a transfer socket", "error", err)
		_ = file.Close()
		s.release(slot)
		s.sendTo(from, encodeError(errNotDefined, "Internal error"))
		return
	}

	opts := negotiate(req, s.limits, false, -1)
	t := &writeTransfer{
		server:        s,
		slot:          slot,
		req:           req,
		peer:          from,
		conn:          conn,
		log:           s.log.With("client", slot.client, "file", sanitize(req.filename)),
		file:          file,
		blockSize:     opts.blockSize,
		timeout:       opts.timeout,
		retries:       s.limits.retries,
		acked:         opts.acked,
		expectedBlock: 1,
	}
	if req.mode == modeNetascii {
		t.netascii = newNetasciiWriter(file)
		t.sink = t.netascii
	} else {
		t.sink = file
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

func (t *writeTransfer) run(ctx context.Context) {
	defer t.cleanup()

	hardDeadline := noDeadline
	if t.server.cfg.TransferTimeout > 0 {
		hardDeadline = time.Now().Add(time.Duration(t.server.cfg.TransferTimeout) * time.Second)
	}
	go func() {
		<-ctx.Done()
		_ = t.conn.SetReadDeadline(time.Now())
	}()

	if len(t.acked) > 0 {
		t.send(encodeOACK(t.acked))
	} else {
		t.send(encodeACK(0))
	}

	for {
		msg, ok := t.await(ctx, hardDeadline)
		if !ok {
			return
		}
		if len(msg) < 4 {
			continue
		}
		op := opcode(uint16(msg[0])<<8 | uint16(msg[1]))
		if op == opERROR {
			t.log.Debug("tftp client aborted the write transfer")
			return
		}
		if op != opDATA {
			t.send(encodeError(errIllegalOperation, "Illegal TFTP operation"))
			return
		}

		block := uint16(msg[2])<<8 | uint16(msg[3])
		data := msg[4:]

		if block != uint16(t.expectedBlock) {
			// duplicate or out of order, acknowledge the last accepted block
			t.send(encodeACK(uint16(t.expectedBlock - 1)))
			continue
		}

		limit := t.server.cfg.MaxFileSize
		if limit > 0 && t.bytesWritten+int64(len(data)) > limit {
			t.log.Debug("tftp write exceeds the maximum size", "maxFileSize", limit)
			t.send(encodeError(errDiskFull, fmt.Sprintf("File exceeds the maximum of %d bytes", limit)))
			return
		}
		if _, err := t.sink.Write(data); err != nil {
			t.log.Debug("tftp write error", "error", err)
			t.send(encodeError(errDiskFull, "Write error"))
			return
		}
		t.bytesWritten += int64(len(data))

		t.send(encodeACK(block))
		t.expectedBlock++

		// the final block is shorter than the block size
		if len(data) < t.blockSize {
			t.finish(ctx)
			return
		}
	}
}

// finish flushes the file and then keeps answering a retransmitted final block
// for one retransmit interval, so that a client whose last acknowledgement was
// lost still gets an answer to its retry.
func (t *writeTransfer) finish(ctx context.Context) {
	if t.netascii != nil {
		if err := t.netascii.Flush(); err != nil {
			t.log.Debug("tftp flush failed", "error", err)
		}
	}
	if err := t.file.Sync(); err != nil {
		t.log.Debug("tftp sync failed", "error", err)
	}
	if err := t.file.Close(); err != nil {
		t.log.Debug("tftp close failed", "error", err)
		t.send(encodeError(errDiskFull, "Write error"))
		return
	}

	t.log.Info("tftp transfer complete",
		"direction", "upload",
		"bytes", t.bytesWritten,
		"blksize", t.blockSize,
		"mode", t.req.mode)

	t.dally(ctx)
}

func (t *writeTransfer) dally(ctx context.Context) {
	deadline := time.Now().Add(t.timeout)
	buf := make([]byte, 65536)
	for {
		if ctx.Err() != nil {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		_ = t.conn.SetReadDeadline(time.Now().Add(remaining))
		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !sameClient(from, t.peer) {
			t.send2(from, encodeError(errUnknownTID, "Unknown transfer ID"))
			continue
		}
		if n < 4 {
			continue
		}
		op := opcode(uint16(buf[0])<<8 | uint16(buf[1]))
		if op == opERROR {
			return
		}
		if op == opDATA {
			// the client did not see the last acknowledgement, send it again
			t.send(encodeACK(uint16(t.expectedBlock - 1)))
		}
	}
}

func (t *writeTransfer) await(ctx context.Context, hardDeadline time.Time) ([]byte, bool) {
	resend := func() {
		if t.expectedBlock == 1 && len(t.acked) > 0 {
			t.send(encodeOACK(t.acked))
			return
		}
		t.send(encodeACK(uint16(t.expectedBlock - 1)))
	}
	return awaitPacket(ctx, t.conn, t.peer, t.log, t.timeout, t.retries, hardDeadline, resend)
}

func (t *writeTransfer) send(packet []byte) {
	t.send2(t.peer, packet)
}

func (t *writeTransfer) send2(to net.Addr, packet []byte) {
	if _, err := t.conn.WriteTo(packet, to); err != nil {
		t.log.Debug("tftp send failed", "error", err)
	}
}

func (t *writeTransfer) cleanup() {
	// Close is idempotent enough here: a finished transfer already closed the
	// file, an aborted one still has to.
	if err := t.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.log.Debug("tftp close failed", "error", err)
	}
	_ = t.conn.Close()
	t.server.release(t.slot)
}
