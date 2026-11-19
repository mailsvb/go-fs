package supervisor

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/logging"
)

// A reload that changes log.level takes effect without a restart: that is how
// debug output is switched on to look at a problem on a running server.
func TestReloadSwitchesTheLogLevel(t *testing.T) {
	// the servers Apply starts log from their own goroutines, the staging
	// sweep among them, while the test reads what has been written so far
	var out lockedBuffer
	root := logging.NewTo(&out, "info", "text")
	sup := New(root.Logger, filepath.Join(t.TempDir(), "go-fs.toml"))
	sup.TrackLog(root)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sup.Shutdown(context.Background())
	})

	cfg := baseConfig(t)
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if root.Level() != slog.LevelInfo {
		t.Fatalf("level after the first apply = %v", root.Level())
	}
	if strings.Contains(out.String(), "log level changed") {
		t.Fatal("the first apply must not report a level change")
	}

	cfg.General.LogLevel = "debug"
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if root.Level() != slog.LevelDebug {
		t.Fatalf("level after the reload = %v, want debug", root.Level())
	}
	if !strings.Contains(out.String(), "log level changed") {
		t.Errorf("the switch has to be reported:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "sections=general") {
		t.Errorf("the reload record has to name the changed section:\n%s", out.String())
	}

	cfg.General.LogFormat = "json"
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "general.logFormat changed") {
		t.Errorf("a format change cannot apply and has to say so:\n%s", out.String())
	}
}

// lockedBuffer is a bytes.Buffer that may be written and read from several
// goroutines at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestChangedSections(t *testing.T) {
	before := config.Default()
	after := before
	if got := changedSections(before, after); len(got) != 0 {
		t.Errorf("identical configurations differ in %v", got)
	}
	after.FTP.Port = 2121
	after.General.LogLevel = "debug"
	if got := changedSections(before, after); !slices.Equal(got, []string{"general", "ftp"}) {
		t.Errorf("changed sections = %v", got)
	}
}
