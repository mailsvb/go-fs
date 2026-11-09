package httpd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An audio that is to be an MP3 is converted by go-fs, not by yt-dlp: yt-dlp
// runs ffmpeg and reads what it says only once it ended, so the minutes ffmpeg
// takes over a long mix would show as nothing at all. Run here with -progress,
// ffmpeg says how far it is about twice a second, which the job shows. The
// arguments are those yt-dlp gives for -x --audio-format mp3 --audio-quality 0.

// mp3Args are the arguments ffmpeg is run with to convert in into an MP3 at
// out, at the highest VBR quality, saying its progress on stdout.
func mp3Args(in, out string) []string {
	return []string{
		"-hide_banner", "-nostdin", "-nostats", "-y",
		"-i", "file:" + in,
		"-vn", "-acodec", "libmp3lame", "-q:a", "0",
		"-f", "mp3", "-progress", "pipe:1",
		"file:" + out,
	}
}

// toMP3 converts the audio yt-dlp downloaded to staged into an MP3 next to
// it, and says where that is. An MP3 is taken as it is.
func (m *mediaRun) toMP3(ctx context.Context, program, staged string) (string, error) {
	ext := filepath.Ext(staged)
	if strings.EqualFold(ext, ".mp3") {
		return staged, nil
	}
	target := strings.TrimSuffix(staged, ext) + ".mp3"
	// mediaFile passes over a .part, so the MP3 counts only once it is whole
	part := target + ".part"

	cmd := exec.CommandContext(ctx, program, mp3Args(staged, part)...)
	cmd.WaitDelay = mediaWaitDelay
	ownProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("ffmpeg cannot be started: %w", err)
	}
	m.job.converting()
	conv := &mediaConversion{job: m.job}
	// ffmpeg says how far it is twice a second even while it reads nothing,
	// so a long silence is an ffmpeg that hangs
	conv.timer = time.AfterFunc(remoteIdleTimeout, func() { m.cancel(errStalled) })
	defer conv.timer.Stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanLines(stdout, conv.progress) }()
	go func() { defer wg.Done(); scanLines(stderr, conv.said) }()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		conv.mu.Lock()
		last := tailLine(conv.stderr)
		conv.mu.Unlock()
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit) && last != "":
			return "", fmt.Errorf("ffmpeg failed with exit status %d: %s", exit.ExitCode(), last)
		case errors.As(err, &exit):
			return "", fmt.Errorf("ffmpeg failed with exit status %d", exit.ExitCode())
		default:
			return "", fmt.Errorf("ffmpeg failed: %w", err)
		}
	}
	if err := os.Rename(part, target); err != nil {
		return "", err
	}
	_ = os.Remove(staged)
	return target, nil
}

// mediaConversion follows one run of ffmpeg: it reads the length of the
// audio from what ffmpeg says on stderr, and how far it is from its progress
// on stdout, into the job.
type mediaConversion struct {
	job   *registryJob
	timer *time.Timer

	mu sync.Mutex
	// total and done are the length of the audio and how much of it is
	// converted, in microseconds; speed is how many times faster than it
	// plays ffmpeg converts.
	total, done int64
	speed       float64
	// stderr is the end of what ffmpeg said there.
	stderr []byte
}

// said takes a line ffmpeg printed on stderr: the first Duration is that of
// the input.
func (c *mediaConversion) said(line string) {
	line = strings.TrimRight(line, "\r")
	c.mu.Lock()
	defer c.mu.Unlock()
	if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Duration: "); ok && c.total == 0 {
		clock, _, _ := strings.Cut(rest, ",")
		c.total = clockMicros(clock)
	}
	c.stderr = appendTail(c.stderr, line)
}

// progress takes a line of what -progress prints, key=value, of which a
// block ends with progress=continue, or progress=end at the end.
func (c *mediaConversion) progress(line string) {
	key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
	if !ok {
		return
	}
	value = strings.TrimSpace(value)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch key {
	case "out_time_us":
		// N/A until the first audio is written
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
			c.done = n
		}
	case "speed":
		// " 24.1x", or N/A at first
		if f, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64); err == nil && f >= 0 {
			c.speed = f
		}
	case "progress":
		done := c.done
		if c.total > 0 {
			done = min(done, c.total)
		}
		c.job.converted(done, c.total, c.speed)
		c.timer.Reset(remoteIdleTimeout)
	}
}

// clockMicros reads a time ffmpeg printed, HH:MM:SS.ss, in microseconds; it
// is 0 for N/A or anything else.
func clockMicros(clock string) int64 {
	parts := strings.Split(strings.TrimSpace(clock), ":")
	if len(parts) != 3 {
		return 0
	}
	hours, err1 := strconv.ParseInt(parts[0], 10, 64)
	minutes, err2 := strconv.ParseInt(parts[1], 10, 64)
	seconds, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil || hours < 0 || minutes < 0 || seconds < 0 {
		return 0
	}
	return (hours*3600+minutes*60)*1_000_000 + int64(seconds*1_000_000)
}

// scanLines hands each line r says to take, and reads and drops whatever is
// left once a line was longer than the buffer, so that the program writing
// to r is not blocked.
func scanLines(r io.Reader, take func(string)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		take(scanner.Text())
	}
	_, _ = io.Copy(io.Discard, r)
}

// appendTail adds line to tail, keeping no more than mediaStderrTail of it.
func appendTail(tail []byte, line string) []byte {
	tail = append(tail, line...)
	tail = append(tail, '\n')
	if over := len(tail) - mediaStderrTail; over > 0 {
		tail = tail[over:]
	}
	return tail
}

// tailLine is the last line of tail, cut short where it is long.
func tailLine(tail []byte) string {
	lines := strings.Split(strings.TrimSpace(string(tail)), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if len(line) > 300 {
		line = line[:300] + "…"
	}
	return line
}
