package httpd

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// cleanupInterval is how often the retention sweep runs, as in the Node
// implementation.
const cleanupInterval = time.Hour

// runCleanup keeps the configured folders from growing without bound. It is
// the one thing in the server that deletes without a client having asked, so
// every removal is reported.
// The list is read at every sweep rather than at the start, so a folder added
// to it by a reload is swept without a restart, and one taken out of it stops
// being swept at once.
func (s *Server) runCleanup(ctx context.Context) {
	s.cleanupOnce()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			s.cleanupOnce()
		}
	}
}

// cleanupOnce sweeps every configured folder once.
func (s *Server) cleanupOnce() {
	entries := s.settings().cfg.Cleanup
	if len(entries) == 0 {
		return
	}
	for _, entry := range entries {
		target := s.root.Resolve("/", entry.Path)
		if !target.Valid {
			s.log.Error("http cleanup path is outside the served folder", "path", entry.Path)
			continue
		}
		listed, err := readDirectory(target.Path)
		if err != nil {
			s.log.Debug("http cleanup cannot read the folder", "path", entry.Path, "error", err)
			continue
		}

		// readDirectory reports folders first and then files newest first, so
		// the files past the ones to keep are the oldest
		files := make([]string, 0, len(listed))
		for _, item := range listed {
			if item.IsFile {
				files = append(files, item.Name)
			}
		}
		if len(files) <= entry.Keep {
			continue
		}
		for _, name := range files[entry.Keep:] {
			path := filepath.Join(target.Path, name)
			if err := os.Remove(path); err != nil {
				s.log.Error("http cleanup cannot remove the file",
					"path", entry.Path+"/"+name, "error", err)
				continue
			}
			s.log.Info("http cleanup removed a file", "path", entry.Path+"/"+name, "keep", entry.Keep)
		}
	}
}
