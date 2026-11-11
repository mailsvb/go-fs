package logging

import (
	"log/slog"
	"net/http"
	"time"
)

// Transport is base with every request it sends and every answer it gets
// recorded at debug: "out http >" with the method, the URL and the header,
// and "out http <" with the status, the header and how long it took. peer
// names what is talked to, "registry" or "sonos". Bodies are never recorded,
// only their length and type, and what authenticates is masked as Headers
// and URL mask it. A redirect the client follows is a request of its own,
// so it is recorded as one, with the Location it came from masked as well.
//
// A request whose context carries a logger (WithLogger) is traced to that
// one, the request's or the job's that sends it; log is for the others, and
// may be nil for a client shared by all of them.
func Transport(log *slog.Logger, peer string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{log: OrDiscard(log), peer: peer, base: base}
}

type transport struct {
	log  *slog.Logger
	peer string
	base http.RoundTripper
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := r.Context()
	log := From(ctx, t.log)
	if !log.Enabled(ctx, slog.LevelDebug) {
		return t.base.RoundTrip(r)
	}
	log = log.With("peer", t.peer)
	log.LogAttrs(ctx, slog.LevelDebug, "out http >",
		slog.String("method", r.Method),
		slog.String("url", URL(r.URL)),
		slog.Any("header", Headers(r.Header)),
		slog.Int64("contentLength", r.ContentLength))
	started := time.Now()
	resp, err := t.base.RoundTrip(r)
	took := time.Since(started).Round(time.Millisecond)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelDebug, "out http failed",
			slog.String("method", r.Method),
			slog.String("url", URL(r.URL)),
			slog.Any("error", err),
			slog.Duration("took", took))
		return nil, err
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("url", URL(r.URL)),
		slog.Int("status", resp.StatusCode),
		slog.String("proto", resp.Proto),
		slog.Any("header", Headers(resp.Header)),
		slog.Int64("contentLength", resp.ContentLength),
		slog.Duration("took", took),
	}
	if resp.TLS != nil {
		attrs = append(attrs, slog.Any("tls", TLS(resp.TLS)))
	}
	log.LogAttrs(ctx, slog.LevelDebug, "out http <", attrs...)
	return resp, nil
}

// CloseIdleConnections passes the call on, so that the client's
// CloseIdleConnections still reaches the transport under the record.
func (t *transport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
