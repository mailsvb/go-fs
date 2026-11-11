//go:build !light && !no_nbd

package nbd

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// The protocol trace, at debug: "nbd <" for each option of the handshake
// and each request of the transmission phase, "nbd >" for each answer.
// Requests name the command, the cookie, the offset and the length; the data
// of a read or a write is never recorded.

func optionName(option uint32) string {
	switch option {
	case optExportName:
		return "EXPORT_NAME"
	case optAbort:
		return "ABORT"
	case optList:
		return "LIST"
	case optStartTLS:
		return "STARTTLS"
	case optInfo:
		return "INFO"
	case optGo:
		return "GO"
	case optStructuredReply:
		return "STRUCTURED_REPLY"
	case optListMetaContext:
		return "LIST_META_CONTEXT"
	case optSetMetaContext:
		return "SET_META_CONTEXT"
	case optExtendedHeaders:
		return "EXTENDED_HEADERS"
	}
	return fmt.Sprintf("option %d", option)
}

func replyName(kind uint32) string {
	switch kind {
	case repAck:
		return "ACK"
	case repServer:
		return "SERVER"
	case repInfo:
		return "INFO"
	case repMetaContext:
		return "META_CONTEXT"
	case repErrUnsup:
		return "ERR_UNSUP"
	case repErrPolicy:
		return "ERR_POLICY"
	case repErrInvalid:
		return "ERR_INVALID"
	case repErrPlatform:
		return "ERR_PLATFORM"
	case repErrTLSReqd:
		return "ERR_TLS_REQD"
	case repErrUnknown:
		return "ERR_UNKNOWN"
	case repErrShutdown:
		return "ERR_SHUTDOWN"
	case repErrTooBig:
		return "ERR_TOO_BIG"
	}
	return fmt.Sprintf("reply %#x", kind)
}

func commandName(kind uint16) string {
	switch kind {
	case cmdRead:
		return "READ"
	case cmdWrite:
		return "WRITE"
	case cmdDisc:
		return "DISC"
	case cmdFlush:
		return "FLUSH"
	case cmdTrim:
		return "TRIM"
	case cmdCache:
		return "CACHE"
	case cmdWriteZeroes:
		return "WRITE_ZEROES"
	case cmdBlockStatus:
		return "BLOCK_STATUS"
	}
	return fmt.Sprintf("command %d", kind)
}

// traceRequest records a request as it was read.
func (c *conn) traceRequest(req *request) {
	if !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "nbd <",
		slog.String("command", commandName(req.kind)),
		slog.Uint64("cookie", req.cookie),
		slog.Uint64("offset", req.offset),
		slog.Uint64("length", uint64(req.length)),
		slog.String("flags", fmt.Sprintf("%#x", req.flags)))
}

// traceAnswer records how a request was answered.
func (c *conn) traceAnswer(req *request, started time.Time) {
	if !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("command", commandName(req.kind)),
		slog.Uint64("cookie", req.cookie),
		slog.Duration("took", time.Since(started).Round(time.Microsecond)),
	}
	if req.failed != 0 {
		attrs = append(attrs, slog.Uint64("error", uint64(req.failed)), slog.String("reason", req.reason))
	} else {
		attrs = append(attrs, slog.String("error", "none"))
	}
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "nbd >", attrs...)
}
