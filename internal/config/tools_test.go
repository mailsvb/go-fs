package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeToolOutput is what the test binary prints and exits with when a test
// runs it as a program of general.tools: it stands in for yt-dlp and ffmpeg,
// which a test cannot count on, on every system alike.
const fakeToolOutput = "GOFS_FAKE_TOOL_OUTPUT"

func TestMain(m *testing.M) {
	if out, ok := os.LookupEnv(fakeToolOutput); ok {
		fmt.Print(out)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeTool is a copy of the test binary under name, in a folder of its own,
// which prints out when it is run.
func fakeTool(t *testing.T, name, out string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeToolOutput, out)
	return path
}

// stubProbe answers for every program with version, or fails for one whose
// path is in bad, and counts how often it was asked.
func stubProbe(t *testing.T, bad map[string]bool) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := probeTool
	probeTool = func(kind ToolKind, path string) (string, error) {
		calls.Add(1)
		if bad[path] {
			return "", errors.New("not it")
		}
		return "1.0", nil
	}
	t.Cleanup(func() { probeTool = old })
	return &calls
}

func noToolDirs(t *testing.T, dirs ...string) {
	t.Helper()
	old := toolDirs
	toolDirs = func() []string { return dirs }
	t.Cleanup(func() { toolDirs = old })
	t.Setenv("PATH", "")
}

func executable(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheProbeKnowsEachProgram(t *testing.T) {
	for _, test := range []struct {
		kind    ToolKind
		out     string
		version string
	}{
		{ToolYtDlp, "2026.10.31\n", "2026.10.31"},
		{ToolYtDlp, "2025.10.22.232808\n", "2025.10.22.232808"},
		{ToolYtDlp, "yt-dlp 2026.10.31\n", ""},
		{ToolYtDlp, "", ""},
		{ToolFFmpeg, "ffmpeg version 7.1.1 Copyright (c) 2000-2025 the FFmpeg developers\nbuilt with clang\n", "7.1.1"},
		{ToolFFmpeg, "ffprobe version 7.1.1 Copyright\n", ""},
		{ToolFFmpeg, "2026.10.31\n", ""},
	} {
		path := fakeTool(t, test.kind.Name, test.out)
		version, err := runProbe(test.kind, path)
		if test.version == "" {
			if err == nil || !strings.Contains(err.Error(), "is not "+test.kind.Name) {
				t.Errorf("%s printing %q: got %q, %v; want it refused", test.kind.Name, test.out, version, err)
			}
			continue
		}
		if err != nil || version != test.version {
			t.Errorf("%s printing %q: got %q, %v; want %q", test.kind.Name, test.out, version, err, test.version)
		}
	}
}

func TestAToolPathThatIsSetHasToBeTheTool(t *testing.T) {
	folder := t.TempDir()
	cfg := servingConfig(folder)

	cfg.General.Tools.YtDlp = fakeTool(t, "yt-dlp", "2026.10.31\n")
	if err := cfg.Validate(); err != nil {
		t.Errorf("yt-dlp is refused: %v", err)
	}
	if tool := cfg.Resolved().HTTP.YtDlp; tool.Path != cfg.General.Tools.YtDlp || tool.Version != "2026.10.31" || !tool.Configured {
		t.Errorf("yt-dlp resolves to %+v", tool)
	}

	cfg.General.Tools.YtDlp = fakeTool(t, "ls", "Makefile\nREADME.md\n")
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "general.tools.ytDlp") || !strings.Contains(err.Error(), "is not yt-dlp") {
		t.Errorf("another program is taken for yt-dlp: %v", err)
	}
	if tool := cfg.Resolved().HTTP.YtDlp; tool.Found() {
		t.Errorf("another program resolves to %+v", tool)
	}

	for _, path := range []string{"yt-dlp", filepath.Join(folder, "missing"), folder} {
		cfg.General.Tools.YtDlp = path
		if err := cfg.Validate(); err == nil {
			t.Errorf("%q is taken for yt-dlp", path)
		}
	}
	cfg.General.Tools.YtDlp = ""
	if runtime.GOOS != "windows" {
		cfg.General.Tools.FFmpeg = executable(t, folder, "ffmpeg")
		if err := os.Chmod(cfg.General.Tools.FFmpeg, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "not executable") {
			t.Errorf("a file that cannot run is taken for ffmpeg: %v", err)
		}
	}
}

func TestAnEmptyToolPathIsLookedFor(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	noToolDirs(t, first, second)
	stubProbe(t, nil)
	if tool := FindTool(ToolYtDlp, ""); tool.Found() {
		t.Errorf("nothing there, and found %+v", tool)
	}

	path := executable(t, second, "yt-dlp")
	if tool := FindTool(ToolYtDlp, ""); tool.Path != path || tool.Configured {
		t.Errorf("found %+v, want %s", tool, path)
	}
	if tool := FindTool(ToolFFmpeg, ""); tool.Found() {
		t.Errorf("yt-dlp is taken for ffmpeg: %+v", tool)
	}
}

func TestTheFirstToolThatAnswersIsTaken(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	noToolDirs(t, first, second)
	wrong := executable(t, first, "yt-dlp")
	right := executable(t, second, "yt-dlp")
	stubProbe(t, map[string]bool{wrong: true})
	if tool := FindTool(ToolYtDlp, ""); tool.Path != right {
		t.Errorf("found %+v, want %s", tool, right)
	}
}

func TestThePathComesBeforeTheUsualFolders(t *testing.T) {
	onPath, usual := t.TempDir(), t.TempDir()
	noToolDirs(t, usual)
	executable(t, usual, "ffmpeg")
	want := executable(t, onPath, "ffmpeg")
	t.Setenv("PATH", onPath)
	stubProbe(t, nil)
	if tool := FindTool(ToolFFmpeg, ""); tool.Path != want {
		t.Errorf("found %+v, want %s", tool, want)
	}
}

func TestAToolIsRunOnceWhileItStaysTheSame(t *testing.T) {
	dir := t.TempDir()
	noToolDirs(t, dir)
	calls := stubProbe(t, nil)
	path := executable(t, dir, "yt-dlp")
	for range 3 {
		FindTool(ToolYtDlp, "")
		FindTool(ToolYtDlp, path)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("run %d times, want once", n)
	}
	// another file at the same path is another program
	if err := os.WriteFile(path, []byte("longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	FindTool(ToolYtDlp, "")
	if n := calls.Load(); n != 2 {
		t.Errorf("a changed file was run %d times in all, want twice", n)
	}
}
