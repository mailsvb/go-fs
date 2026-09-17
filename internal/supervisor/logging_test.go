package supervisor

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go-fs/internal/config"
	"go-fs/internal/logging"
)

// A reload that changes log.level takes effect without a restart: that is how
// debug output is switched on to look at a problem on a running server.
func TestReloadSwitchesTheLogLevel(t *testing.T) {
	var out bytes.Buffer
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

	cfg.Log.Level = "debug"
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if root.Level() != slog.LevelDebug {
		t.Fatalf("level after the reload = %v, want debug", root.Level())
	}
	if !strings.Contains(out.String(), "log level changed") {
		t.Errorf("the switch has to be reported:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "sections=log") {
		t.Errorf("the reload record has to name the changed section:\n%s", out.String())
	}

	cfg.Log.Format = "json"
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "log.format changed") {
		t.Errorf("a format change cannot apply and has to say so:\n%s", out.String())
	}
}

func TestChangedSections(t *testing.T) {
	before := config.Default()
	after := before
	if got := changedSections(before, after); len(got) != 0 {
		t.Errorf("identical configurations differ in %v", got)
	}
	after.FTP.Port = 2121
	after.Log.Level = "debug"
	if got := changedSections(before, after); !slices.Equal(got, []string{"log", "ftp"}) {
		t.Errorf("changed sections = %v", got)
	}
}
