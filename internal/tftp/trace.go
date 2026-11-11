package tftp

import (
	"context"
	"encoding/binary"
	"log/slog"
	"strings"
	"sync"
)

// The protocol trace: "tftp <" for a packet that came in and "tftp >" for one
// that went out, at debug. Every request, OACK and ERROR is recorded. DATA
// and ACK are not all recorded, since a transfer of a few megabytes in blocks
// of 512 bytes would be thousands of lines that all say the same: only the
// first block of each, the last DATA (the one shorter than a block), a DATA
// that is not the block after the one before it (a retransmission or one out
// of order), and an ACK that does not move forward (a duplicate) are. Those
// are the packets that say how a transfer went.

// packetTrace traces the packets of one transfer.
type packetTrace struct {
	log *slog.Logger
	// blockSize is what the transfer agreed on, which tells the last DATA
	mu        sync.Mutex
	blockSize int
	in, out   flow
}

// flow is the last DATA and ACK seen in one direction.
type flow struct {
	data, ack         uint16
	seenData, seenAck bool
}

func newPacketTrace(log *slog.Logger, blockSize int) *packetTrace {
	return &packetTrace{log: log, blockSize: blockSize}
}

// received traces a packet that came in, sent one that went out.
func (p *packetTrace) received(msg []byte) { p.packet("<", msg) }
func (p *packetTrace) sent(msg []byte)     { p.packet(">", msg) }

func (p *packetTrace) packet(dir string, msg []byte) {
	if p == nil || !p.log.Enabled(context.Background(), slog.LevelDebug) || len(msg) < 2 {
		return
	}
	p.mu.Lock()
	f := &p.in
	if dir == ">" {
		f = &p.out
	}
	shown := p.worthShowing(f, msg)
	p.mu.Unlock()
	if shown {
		p.log.LogAttrs(context.Background(), slog.LevelDebug, "tftp "+dir, describePacket(msg)...)
	}
}

// worthShowing decides whether a DATA or an ACK is one the trace keeps, and
// remembers it for the next.
func (p *packetTrace) worthShowing(f *flow, msg []byte) bool {
	op := opcode(binary.BigEndian.Uint16(msg))
	if (op != opDATA && op != opACK) || len(msg) < 4 {
		return true
	}
	block := binary.BigEndian.Uint16(msg[2:])
	switch op {
	case opDATA:
		// the arithmetic wraps as the block number does after 65535
		shown := !f.seenData || block != f.data+1 || len(msg)-4 < p.blockSize
		f.data, f.seenData = block, true
		return shown
	default:
		// an ACK that does not move past the one before is a duplicate or a
		// stale one, which is what sets off retransmissions; the difference
		// is taken as signed so that it holds across the wrap as well
		shown := !f.seenAck || int16(block-f.ack) <= 0
		f.ack, f.seenAck = block, true
		return shown
	}
}

// tracePacket records a packet of the request socket, all of which are kept.
func tracePacket(log *slog.Logger, dir, client string, msg []byte) {
	if !log.Enabled(context.Background(), slog.LevelDebug) || len(msg) < 2 {
		return
	}
	attrs := append([]slog.Attr{slog.String("client", client)}, describePacket(msg)...)
	log.LogAttrs(context.Background(), slog.LevelDebug, "tftp "+dir, attrs...)
}

// describePacket is what the trace says of a packet: its type and its
// header, never the data it carries.
func describePacket(msg []byte) []slog.Attr {
	op := opcode(binary.BigEndian.Uint16(msg))
	attrs := []slog.Attr{slog.String("packet", op.String())}
	switch op {
	case opRRQ, opWRQ:
		if req, ok := parseRequest(msg); ok {
			attrs = append(attrs, slog.String("file", sanitize(req.filename)),
				slog.String("mode", sanitize(req.mode)))
			if len(req.options) > 0 {
				attrs = append(attrs, slog.String("options", showOptions(req.options)))
			}
		} else {
			attrs = append(attrs, slog.Bool("malformed", true))
		}
	case opDATA:
		if len(msg) >= 4 {
			attrs = append(attrs, slog.Int("block", int(binary.BigEndian.Uint16(msg[2:]))),
				slog.Int("bytes", len(msg)-4))
		}
	case opACK:
		if len(msg) >= 4 {
			attrs = append(attrs, slog.Int("block", int(binary.BigEndian.Uint16(msg[2:]))))
		}
	case opERROR:
		if len(msg) >= 4 {
			attrs = append(attrs, slog.Int("code", int(binary.BigEndian.Uint16(msg[2:]))),
				slog.String("message", sanitize(strings.TrimRight(string(msg[4:]), "\x00"))))
		}
	case opOACK:
		attrs = append(attrs, slog.String("options", showOptions(parseOptions(msg[2:]))))
	default:
		attrs = append(attrs, slog.Int("opcode", int(op)), slog.Int("bytes", len(msg)))
	}
	return attrs
}

// parseOptions reads the key\0value\0 pairs of an OACK.
func parseOptions(body []byte) []option {
	var options []option
	for len(body) > 0 {
		end := indexZero(body)
		if end < 0 {
			break
		}
		key := latin1(body[:end])
		body = body[end+1:]
		end = indexZero(body)
		if end < 0 {
			break
		}
		options = append(options, option{key: key, value: latin1(body[:end])})
		body = body[end+1:]
	}
	return options
}

func showOptions(options []option) string {
	shown := make([]string, len(options))
	for i, opt := range options {
		shown[i] = sanitize(opt.key) + "=" + sanitize(opt.value)
	}
	return strings.Join(shown, " ")
}

func (op opcode) String() string {
	switch op {
	case opRRQ:
		return "RRQ"
	case opWRQ:
		return "WRQ"
	case opDATA:
		return "DATA"
	case opACK:
		return "ACK"
	case opERROR:
		return "ERROR"
	case opOACK:
		return "OACK"
	}
	return "unknown"
}
