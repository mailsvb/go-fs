package tftp

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Of the DATA and ACK of a transfer only those that say how it went are
// traced: the first, the last, a retransmission and a duplicate ACK.
func TestTheTraceKeepsTheTellingPackets(t *testing.T) {
	var out bytes.Buffer
	trace := newPacketTrace(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})), 4)
	full := []byte("abcd")
	for _, block := range []uint16{1, 2, 3, 3, 4} {
		trace.sent(encodeDATA(block, full))
	}
	trace.sent(encodeDATA(5, []byte("ab")))
	for _, block := range []uint16{1, 2, 3, 3, 4} {
		trace.received(encodeACK(block))
	}
	trace.sent(encodeError(errDiskFull, "full"))

	var shown []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		packet := between(line, "packet=", " ")
		block := between(line, "block=", " ")
		shown = append(shown, packet+block)
	}
	want := []string{"DATA1", "DATA3", "DATA5", "ACK1", "ACK3", "ERROR"}
	if strings.Join(shown, ",") != strings.Join(want, ",") {
		t.Errorf("traced %v, want %v\n%s", shown, want, out.String())
	}
}

func between(s, from, to string) string {
	_, rest, ok := strings.Cut(s, from)
	if !ok {
		return ""
	}
	value, _, _ := strings.Cut(rest, to)
	return value
}
