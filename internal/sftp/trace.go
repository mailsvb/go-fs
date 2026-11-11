//go:build !light && !no_sftpd

package sftp

import (
	"io"
	"log/slog"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// The four entry points the library calls are traced here, around the code
// that serves them: "sftp <" names the request as it comes in, and "sftp >"
// says how it ended and how long that took, so that a request is never seen
// to start without being seen to end. The bytes a read or a write moves are
// in the record written when its handle closes.

// Fileread implements sftp.FileReader.
func (s *session) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	s.trace(r)
	started := time.Now()
	reader, err := s.fileread(r)
	s.done(r, started, err)
	return reader, err
}

// Filewrite implements sftp.FileWriter.
func (s *session) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	s.trace(r)
	started := time.Now()
	writer, err := s.filewrite(r)
	s.done(r, started, err)
	return writer, err
}

// Filecmd implements sftp.FileCmder.
func (s *session) Filecmd(r *sftp.Request) error {
	s.trace(r)
	started := time.Now()
	err := s.filecmd(r)
	s.done(r, started, err)
	return err
}

// Filelist implements sftp.FileLister.
func (s *session) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	s.trace(r)
	started := time.Now()
	lister, err := s.filelist(r)
	if list, ok := lister.(listerAt); ok && err == nil {
		s.done(r, started, nil, slog.Int("entries", len(list)))
	} else {
		s.done(r, started, err)
	}
	return lister, err
}

// done writes the "sftp >" of a request: ok, or the error the client is
// told, which the library turns into an SFTP status.
func (s *session) done(r *sftp.Request, started time.Time, err error, attrs ...slog.Attr) {
	if !s.log.Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	all := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.Filepath),
		slog.Duration("took", time.Since(started).Round(time.Millisecond)),
	}
	if err != nil {
		all = append(all, slog.String("status", "error"), slog.Any("error", err))
	} else {
		all = append(all, slog.String("status", "ok"))
	}
	all = append(all, attrs...)
	s.log.LogAttrs(r.Context(), slog.LevelDebug, "sftp >", all...)
}

// algorithms is what a record says of what an SSH handshake agreed on.
func algorithms(a ssh.NegotiatedAlgorithms) slog.Value {
	return slog.GroupValue(
		slog.String("kex", a.KeyExchange),
		slog.String("hostKey", a.HostKey),
		slog.String("cipherIn", a.Read.Cipher),
		slog.String("macIn", a.Read.MAC),
		slog.String("cipherOut", a.Write.Cipher),
		slog.String("macOut", a.Write.MAC))
}

// discardRequests refuses the global requests of a connection, as
// ssh.DiscardRequests does, recording each one: a keepalive, or a client
// that asks for a port to be forwarded.
func discardRequests(requests <-chan *ssh.Request, log *slog.Logger) {
	for req := range requests {
		log.Debug("ssh <", "request", req.Type, "wantReply", req.WantReply, "bytes", len(req.Payload))
		if req.WantReply {
			_ = req.Reply(false, nil)
		}
	}
}
