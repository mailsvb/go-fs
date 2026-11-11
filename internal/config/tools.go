package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailsvb/go-fs/internal/logging"
)

// The programs of general.tools are found here: a path that is set is checked,
// an empty one is looked for, and either way a candidate counts only once it
// has been run with its version flag and answered as the program it is meant
// to be. A file of another name, a script that happens to be called yt-dlp, a
// binary that does not start on this machine: none switches anything on.

// ToolKind is one program go-fs may use.
type ToolKind struct {
	// Name is the name of the program, which is also the file looked for.
	Name string
	// Key is its key in general.tools.
	Key string
	// args are what it is run with to say its version.
	args []string
	// version reads the version off the first line it printed, and fails
	// when that line is not what this program prints.
	version func(line string) (string, bool)
}

var ytDlpVersion = regexp.MustCompile(`^\d{4}\.\d{1,2}\.\d{1,2}(\.\d+)?$`)

var (
	// ToolYtDlp is yt-dlp, whose --version is the date of its release, such
	// as 2026.10.31, and nothing else.
	ToolYtDlp = ToolKind{Name: "yt-dlp", Key: "ytDlp", args: []string{"--version"},
		version: func(line string) (string, bool) {
			return line, ytDlpVersion.MatchString(line)
		}}
	// ToolFFmpeg is ffmpeg, whose -version begins "ffmpeg version 7.1 ...".
	ToolFFmpeg = ToolKind{Name: "ffmpeg", Key: "ffmpeg", args: []string{"-version"},
		version: func(line string) (string, bool) {
			rest, ok := strings.CutPrefix(line, "ffmpeg version ")
			if !ok {
				return "", false
			}
			version, _, _ := strings.Cut(rest, " ")
			return version, version != ""
		}}
)

// Tool is a program that was found, by the path it was found at and the
// version it reported. The zero Tool is one that was not found.
type Tool struct {
	Path    string
	Version string
	// Configured says the path came from general.tools rather than from
	// looking for it.
	Configured bool
}

// Found reports whether the program is there.
func (t Tool) Found() bool { return t.Path != "" }

// toolTimeout is how long a program has to say its version. yt-dlp, built as
// one file, unpacks itself before it answers, which takes a few seconds on
// the first run on a slow disk.
const toolTimeout = 15 * time.Second

// probeTool runs a program to learn whether it is the one it is meant to be,
// and reports its version: runProbe, but for a test, which has no yt-dlp.
var probeTool = runProbe

// toolDirs are the folders a program is looked for in after the PATH: those
// that package managers install into, which the PATH of a service, under
// launchd or systemd, often lacks. A test replaces them.
var toolDirs = defaultToolDirs

func defaultToolDirs() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		var dirs []string
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			dirs = append(dirs, filepath.Join(local, "Microsoft", "WinGet", "Links"))
		}
		if home != "" {
			dirs = append(dirs, filepath.Join(home, "scoop", "shims"))
		}
		if data := os.Getenv("ProgramData"); data != "" {
			dirs = append(dirs, filepath.Join(data, "chocolatey", "bin"))
		}
		return dirs
	}
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/snap/bin"}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return dirs
}

// probeKey is a program as it is on disk: the same path with another size or
// time is another program, which is run again.
type probeKey struct {
	kind    string
	path    string
	size    int64
	modTime time.Time
}

type probeResult struct {
	version string
	err     error
}

var (
	probeMu    sync.Mutex
	probeCache = map[probeKey]probeResult{}
	// probeLog takes the trace of the programs run to probe them. The first
	// probes run while the configuration is loaded at the start, before
	// there is a logger; what they found is cached, and logged by the
	// supervisor, so only a probe after that is traced.
	probeLog atomic.Pointer[slog.Logger]
)

// TraceProbes has the probes of the programs of general.tools traced to
// log from now on.
func TraceProbes(log *slog.Logger) {
	probeLog.Store(log)
}

// checkTool decides whether the file at path is the program, and reports its
// version. What a file answered is remembered for as long as it stays the
// same file, so that a reload, which resolves the configuration again, does
// not run every program again.
func checkTool(kind ToolKind, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q has to be an absolute path", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s does not exist", path)
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", path)
	}
	key := probeKey{kind: kind.Name, path: path, size: info.Size(), modTime: info.ModTime()}
	probeMu.Lock()
	result, ok := probeCache[key]
	probeMu.Unlock()
	if ok {
		return result.version, result.err
	}
	version, err := probeTool(kind, path)
	probeMu.Lock()
	probeCache[key] = probeResult{version: version, err: err}
	probeMu.Unlock()
	return version, err
}

// runProbe runs the program with its version flag and reads what it says.
func runProbe(kind ToolKind, path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, kind.args...)
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, limit: 4096}
	cmd.WaitDelay = 2 * time.Second
	err := logging.Run(probeLog.Load(), cmd)
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s did not answer %s within %s", path, strings.Join(kind.args, " "), toolTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("%s is not %s: %s failed: %v", path, kind.Name, strings.Join(kind.args, " "), err)
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	line = strings.TrimSpace(line)
	version, ok := kind.version(line)
	if !ok {
		shown := line
		if len(shown) > 80 {
			shown = shown[:80] + "…"
		}
		return "", fmt.Errorf("%s is not %s: %s printed %q", path, kind.Name, strings.Join(kind.args, " "), shown)
	}
	return version, nil
}

// limitedBuffer keeps the first limit bytes written to it and drops the rest,
// so that a program that prints without end cannot fill the memory.
type limitedBuffer struct {
	buf   *bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

// FindTool is the program as general.tools has it: the path that is set, when
// it is the program, or else the first the PATH and the usual folders hold
// that is. A path that is set but is not the program finds nothing: Validate
// refuses such a file, so this happens only to a configuration that was never
// validated.
func FindTool(kind ToolKind, configured string) Tool {
	if configured != "" {
		version, err := checkTool(kind, configured)
		if err != nil {
			return Tool{}
		}
		return Tool{Path: configured, Version: version, Configured: true}
	}
	for _, candidate := range toolCandidates(kind) {
		if version, err := checkTool(kind, candidate); err == nil {
			return Tool{Path: candidate, Version: version}
		}
	}
	return Tool{}
}

// toolCandidates are the files a program may be, in the order they are
// tried, each once.
func toolCandidates(kind ToolKind) []string {
	var candidates []string
	seen := map[string]bool{}
	add := func(path string) {
		if path == "" || !filepath.IsAbs(path) {
			return
		}
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			candidates = append(candidates, path)
		}
	}
	// a program the PATH finds in the current directory is no candidate:
	// that is where a downloaded file would be, not an installed one
	path, err := exec.LookPath(kind.Name)
	logging.Syscall(probeLog.Load(), "lookpath", err, slog.String("program", kind.Name), slog.String("found", path))
	if err == nil {
		add(path)
	}
	file := kind.Name
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	for _, dir := range toolDirs() {
		add(filepath.Join(dir, file))
	}
	return candidates
}

// validate checks the paths that are set: each has to be the program it is
// set for. An empty path is looked for, and finding nothing is no error.
func (t Tools) validate() error {
	for _, tool := range []struct {
		kind ToolKind
		path string
	}{{ToolYtDlp, t.YtDlp}, {ToolFFmpeg, t.FFmpeg}} {
		if tool.path == "" {
			continue
		}
		if _, err := checkTool(tool.kind, tool.path); err != nil {
			return fmt.Errorf("general.tools.%s: %w", tool.kind.Key, err)
		}
	}
	return nil
}
