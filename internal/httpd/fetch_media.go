package httpd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mailsvb/go-fs/internal/vfs"
)

// A fetch can also take the video or the audio of a page, a YouTube video or
// any of the many other sites yt-dlp knows, rather than the page itself. The
// server runs yt-dlp, which general.tools names or go-fs found, with ffmpeg
// where there is one: it downloads into a folder of its own in the staging
// folder, and the one file it ends up with is stored like any fetched file.
// What the page sees is the same job as a plain fetch: the progress, Stop and
// the Transfers in the header.
//
// yt-dlp is given nothing but the URL and what it is to make of it. A login
// and headers would be on its command line, where every user of the machine
// reads them, so a video or audio fetch takes neither; the configuration
// files yt-dlp would read are ignored, so that what it does is what this file
// says.

const (
	mediaVideo = "video"
	mediaAudio = "audio"
)

// mediaWaitDelay is how long the output of yt-dlp is read after it was
// stopped, before the pipes are closed on whatever still holds them.
const mediaWaitDelay = 5 * time.Second

// mediaStderrTail is how much of what yt-dlp says on stderr is kept, for the
// reason it failed.
const mediaStderrTail = 16 << 10

// mediaProgress begins the lines yt-dlp prints its progress as, which
// --progress-template makes: the bytes downloaded, the total, the estimate of
// the total where the total is not known, and the title, last since it may
// hold anything.
const mediaProgress = "go-fs-progress|"

var mediaProgressTemplate = "download:" + mediaProgress +
	"%(progress.downloaded_bytes)s|%(progress.total_bytes)s|%(progress.total_bytes_estimate)s|%(info.title)s"

// mediaArgs are the arguments yt-dlp is run with to store into work what
// fetch asks of url.
func mediaArgs(set *settings, fetch fetchRequest, work string) []string {
	args := []string{
		"--ignore-config", "--no-playlist", "--max-downloads", "1",
		"--no-mtime", "--no-colors", "--newline", "--progress",
		"--progress-template", mediaProgressTemplate,
		"--socket-timeout", "60",
		"--cache-dir", filepath.Join(stagingFolder(set), "yt-dlp-cache"),
		"-P", work,
		"-o", "%(title).180B [%(id)s].%(ext)s",
	}
	if limit := set.cfg.MaxUploadSize; limit > 0 {
		args = append(args, "--max-filesize", strconv.FormatInt(limit, 10))
	}
	if fetch.skipVerify {
		args = append(args, "--no-check-certificates")
	}
	ffmpeg := set.cfg.FFmpeg
	if ffmpeg.Found() {
		args = append(args, "--ffmpeg-location", ffmpeg.Path)
	}
	switch {
	case fetch.media == mediaAudio && ffmpeg.Found():
		args = append(args, "-f", "ba/b", "-x", "--audio-format", "mp3", "--audio-quality", "0")
	case fetch.media == mediaAudio:
		// without ffmpeg nothing can be converted, so the audio is taken as
		// the site sends it, M4A where it offers that
		args = append(args, "-f", "ba[ext=m4a]/ba")
	case ffmpeg.Found():
		args = append(args, "-f", "bv*+ba/b", "--merge-output-format", "mp4")
	default:
		// the best file that holds both, which needs no ffmpeg to join
		args = append(args, "-f", "b")
	}
	return append(args, "--", fetch.url.String())
}

// mediaJobName is what a video or audio fetch is listed as until yt-dlp
// says the title: the id of a YouTube video, or else the last segment of the
// URL, or its host.
func mediaJobName(u *url.URL) string {
	if id := u.Query().Get("v"); id != "" {
		return id
	}
	if name := cleanFetchedName(u.Path); name != "" {
		return name
	}
	return u.Host
}

// fetchMedia has yt-dlp download the video or the audio of a page into a
// folder, and says what it stored.
func (s *Server) fetchMedia(ctx context.Context, set *settings, job *registryJob, fetch fetchRequest,
	folder string, user *account, address string) (string, error) {
	started := time.Now()
	staging := stagingFolder(set)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(staging, "media-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(work) }()

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	run := &mediaRun{job: job, cancel: cancel}
	if err := run.ytDlp(runCtx, set.cfg.YtDlp.Path, mediaArgs(set, fetch, work)); err != nil {
		if cause := context.Cause(runCtx); errors.Is(cause, errStalled) {
			return "", fmt.Errorf("%s from %s: %w", job.view().Name, fetch.url.Host, errStalled)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	if run.tooLarge() {
		return "", fetchTooLarge(job.view().Name, set.cfg.MaxUploadSize)
	}

	staged, err := mediaFile(work)
	if err != nil {
		return "", err
	}
	name := cleanFetchedName(filepath.Base(staged))
	if name == "" {
		return "", fmt.Errorf("yt-dlp named the file %q, which cannot be stored", filepath.Base(staged))
	}
	job.named(name, "")
	target := s.root.Resolve(folder, name)
	if !target.Valid || vfs.AsFolder(path.Dir(target.Virtual)) != folder {
		return "", fmt.Errorf("a file named %q cannot be stored here", name)
	}
	info, err := os.Stat(staged)
	if err != nil {
		return "", err
	}
	size := info.Size()
	if limit := set.cfg.MaxUploadSize; limit > 0 && size > limit {
		return "", fetchTooLarge(name, limit)
	}
	// the bytes are in the staging folder already, so only the folder they
	// go to has to have room
	if err := checkRoom(set, filepath.Dir(target.Path), folder, size, 0); err != nil {
		return "", err
	}
	// a temporary folder is the owner's alone, and what yt-dlp wrote into it
	// is about to be a file of the served folder like an upload
	_ = os.Chmod(staged, 0o644)
	replace, err := s.placeFetched(target, name, staged, user)
	if err != nil {
		return "", err
	}
	job.plan(0, size)
	job.bytesDone.Store(size)
	s.log.Info("http fetch", "user", user.name, "url", shownURL(fetch.url), "media", fetch.media,
		"file", target.Virtual, "bytes", size, "replaced", replace, "address", address,
		"took", time.Since(started).Round(time.Millisecond))
	return fmt.Sprintf("Fetched %s (%s) into %s.", name, readableSize(size), folder), nil
}

// mediaFile is the one file yt-dlp left in its folder once it ended. What it
// leaves on the way, the parts of a download and the video and audio before
// they were joined, it removes as it goes, so that there is one.
func mediaFile(work string) (string, error) {
	entries, err := os.ReadDir(work)
	if err != nil {
		return "", err
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") {
			continue
		}
		files = append(files, filepath.Join(work, name))
	}
	switch len(files) {
	case 0:
		return "", errors.New("yt-dlp ended without storing a file")
	case 1:
		return files[0], nil
	default:
		return "", fmt.Errorf("yt-dlp stored %d files where one was expected", len(files))
	}
}

// mediaRun follows one run of yt-dlp: it reads the progress yt-dlp prints
// into the job, and keeps what it says went wrong.
type mediaRun struct {
	job    *registryJob
	cancel context.CancelCauseFunc

	mu sync.Mutex
	// begun says the first progress line came, which is when the dialog
	// lets go of the fetch: an error before it is shown there.
	begun bool
	// done is the bytes of the downloads that ended, a video and its audio
	// being two, and last the progress of the one that runs.
	done                int64
	lastBytes, lastSize int64
	// failure is the last ERROR line yt-dlp printed, and larger says it
	// skipped the download for being larger than --max-filesize.
	failure string
	larger  bool
	// local says yt-dlp is past the download, joining or converting what
	// it has, which reads and writes the disk only and goes for a long while
	// without a line: no stall is watched for then.
	local bool
	// stderr is the end of what yt-dlp said there.
	stderr []byte
	timer  *time.Timer
}

// yt-dlp runs yt-dlp to its end, and fails with what it said went wrong.
func (m *mediaRun) ytDlp(ctx context.Context, program string, args []string) error {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = mediaWaitDelay
	ownProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("yt-dlp cannot be started: %w", err)
	}
	m.timer = time.AfterFunc(remoteIdleTimeout, func() { m.cancel(errStalled) })
	defer m.timer.Stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.read(stdout, false) }()
	go func() { defer wg.Done(); m.read(stderr, true) }()
	wg.Wait()
	err = cmd.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 101:
		// --max-downloads ends with 101 once it has its one download
		return nil
	case m.failure != "":
		return errors.New(m.failure)
	case errors.As(err, &exit) && m.lastStderr() != "":
		return fmt.Errorf("yt-dlp failed with exit status %d: %s", exit.ExitCode(), m.lastStderr())
	case errors.As(err, &exit):
		return fmt.Errorf("yt-dlp failed with exit status %d", exit.ExitCode())
	default:
		return fmt.Errorf("yt-dlp failed: %w", err)
	}
}

// read takes what yt-dlp prints, line by line.
func (m *mediaRun) read(r io.Reader, isStderr bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		m.line(scanner.Text(), isStderr)
	}
	// a line longer than the buffer stops the scanner; what is left is
	// read and dropped, so that yt-dlp is not blocked writing it
	_, _ = io.Copy(io.Discard, r)
}

func (m *mediaRun) line(line string, isStderr bool) {
	line = strings.TrimRight(line, "\r")
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.local {
		m.timer.Reset(remoteIdleTimeout)
	}
	if rest, ok := strings.CutPrefix(line, mediaProgress); ok {
		m.progress(rest)
		return
	}
	switch {
	case strings.HasPrefix(line, "ERROR: "):
		m.failure = strings.TrimSpace(strings.TrimPrefix(line, "ERROR: "))
	case strings.Contains(line, "larger than max-filesize"):
		m.larger = true
	case strings.HasPrefix(line, "[Merger]"), strings.HasPrefix(line, "[ExtractAudio]"),
		strings.HasPrefix(line, "[VideoConvertor]"), strings.HasPrefix(line, "[Fixup"):
		m.local = true
		m.timer.Stop()
	}
	if isStderr {
		m.stderr = append(m.stderr, line...)
		m.stderr = append(m.stderr, '\n')
		if over := len(m.stderr) - mediaStderrTail; over > 0 {
			m.stderr = m.stderr[over:]
		}
	}
}

// progress reads one progress line into the job. The video and the audio of
// a video are downloaded one after the other, each counting from zero, so a
// count that falls is the next download, and the one before is added to what
// is done.
func (m *mediaRun) progress(fields string) {
	parts := strings.SplitN(fields, "|", 4)
	if len(parts) < 4 {
		return
	}
	bytes := number(parts[0])
	size := number(parts[1])
	if size <= 0 {
		size = number(parts[2])
	}
	if bytes < m.lastBytes {
		m.done += max(m.lastSize, m.lastBytes)
	}
	m.lastBytes, m.lastSize = bytes, size
	if title := strings.TrimSpace(parts[3]); title != "" && title != "NA" {
		m.job.named(title, "")
	}
	if size > 0 {
		m.job.plan(0, m.done+max(size, bytes))
	}
	m.job.bytesDone.Store(m.done + bytes)
	m.job.sample(time.Now())
	if !m.begun {
		m.begun = true
		m.job.downloading()
	}
}

// lastStderr is the last line yt-dlp said on stderr, for a failure it gave
// no ERROR line for. The caller holds m.mu.
func (m *mediaRun) lastStderr() string {
	lines := strings.Split(strings.TrimSpace(string(m.stderr)), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	return line
}

func (m *mediaRun) tooLarge() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.larger
}

// number reads a count yt-dlp printed, which is NA where it does not know it
// and may be a float for an estimate.
func number(text string) int64 {
	text = strings.TrimSpace(text)
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil && f >= 0 {
		return int64(f)
	}
	return 0
}
