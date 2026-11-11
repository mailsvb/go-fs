package logging

import (
	"context"
	"log/slog"
	"runtime"
	"time"
)

// Discard is a logger that writes nothing, for a package handed no logger.
var Discard = slog.New(slog.DiscardHandler)

// OrDiscard is log, or Discard when there is none, so that a package can
// take an optional logger and log without checking it every time.
func OrDiscard(log *slog.Logger) *slog.Logger {
	if log == nil {
		return Discard
	}
	return log
}

// Debug writes a debug record that says it was written skip frames above
// the caller of Debug, so that a helper which logs on behalf of its caller
// names the caller's file and line in source, not its own. Nothing is done,
// not even finding the frame, when debug is off.
func Debug(ctx context.Context, log *slog.Logger, skip int, msg string, attrs ...slog.Attr) {
	if log == nil || !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	var pcs [1]uintptr
	// 0 is Callers itself, 1 is Debug and 2 the caller of Debug
	runtime.Callers(skip+2, pcs[:])
	record := slog.NewRecord(time.Now(), slog.LevelDebug, msg, pcs[0])
	record.AddAttrs(attrs...)
	_ = log.Handler().Handle(ctx, record)
}

// FileOp records one access to a file: "file <op>" with the path, the error
// when it failed and whatever else the caller knows for free, the size or
// the target of a rename. It is written next to the os call it records.
func FileOp(log *slog.Logger, op, path string, err error, attrs ...slog.Attr) {
	fileOp(log, 1, op, path, err, attrs...)
}

// fileOp is FileOp for a caller skip frames above fileOp's caller.
func fileOp(log *slog.Logger, skip int, op, path string, err error, attrs ...slog.Attr) {
	if log == nil || !log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	all := make([]slog.Attr, 0, len(attrs)+2)
	all = append(all, slog.String("path", path))
	all = append(all, attrs...)
	if err != nil {
		all = append(all, slog.Any("error", err))
	}
	Debug(context.Background(), log, skip+1, "file "+op, all...)
}

// Syscall records a call into the operating system that is not a plain file
// access: "syscall <name>" with what it was asked and the error when it
// failed.
func Syscall(log *slog.Logger, name string, err error, attrs ...slog.Attr) {
	if log == nil || !log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	if err != nil {
		// clipped, so the caller's slice is never written to
		attrs = append(attrs[:len(attrs):len(attrs)], slog.Any("error", err))
	}
	Debug(context.Background(), log, 1, "syscall "+name, attrs...)
}

type loggerKey struct{}

// WithLogger is ctx carrying log, the logger of one request, which handlers
// far from where the request came in take with From.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// From is the logger ctx carries, or fallback when it carries none.
func From(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if log, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && log != nil {
		return log
	}
	return fallback
}

// Ensure is ctx carrying a logger: the one it carries already, which is the
// more specific, or else log.
func Ensure(ctx context.Context, log *slog.Logger) context.Context {
	if From(ctx, nil) != nil || log == nil {
		return ctx
	}
	return WithLogger(ctx, log)
}
