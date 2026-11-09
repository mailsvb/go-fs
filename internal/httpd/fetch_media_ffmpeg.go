package httpd

import (
	"bufio"
	"context"
	"encoding/json"
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
//
// The same run tags the MP3: yt-dlp leaves in meta/ of its folder the
// thumbnail of the video and a few fields of what it knows of it, and ffmpeg
// writes them as ID3v2.3, which more players read than 2.4, the thumbnail
// cut to its middle square as the front cover. A video that names no artist,
// as most do, takes its channel for one.

// mediaMeta is the folder in the work folder of a fetch that yt-dlp writes
// the thumbnail and the fields of the video to; mediaFile looks no deeper
// than the work folder, so neither is taken for the download.
const mediaMeta = "meta"

// mediaTagsTemplate has yt-dlp write the fields of the video the tags are
// made of as one JSON object, those it does not know left out or null.
const mediaTagsTemplate = "%(.{title,track,artist,artists,creator,uploader,channel,album,album_artist," +
	"track_number,release_year,release_date,upload_date,webpage_url})j"

// mediaFields are the fields of a video yt-dlp wrote to meta/tags.json.
// Each is an any, since what yt-dlp knows of a site is not always the type
// YouTube gives it.
type mediaFields struct {
	Title       any `json:"title"`
	Track       any `json:"track"`
	Artist      any `json:"artist"`
	Artists     any `json:"artists"`
	Creator     any `json:"creator"`
	Uploader    any `json:"uploader"`
	Channel     any `json:"channel"`
	Album       any `json:"album"`
	AlbumArtist any `json:"album_artist"`
	TrackNumber any `json:"track_number"`
	ReleaseYear any `json:"release_year"`
	ReleaseDate any `json:"release_date"`
	UploadDate  any `json:"upload_date"`
	WebpageURL  any `json:"webpage_url"`
}

// mediaTags are the ID3 tags of an MP3 made of a video, as ffmpeg names
// them; an empty one is not written.
type mediaTags struct {
	Title, Artist, AlbumArtist, Album, Track, Date, Comment string
}

// readMediaTags makes the tags of the MP3 of what yt-dlp wrote to
// meta/tags.json. Without the file, or with one it cannot read, there are
// none: a fetch is not failed for its tags.
func readMediaTags(work string) mediaTags {
	data, err := os.ReadFile(filepath.Join(work, mediaMeta, "tags.json"))
	if err != nil {
		return mediaTags{}
	}
	// yt-dlp appends a line for each video it printed, of which there is one
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	var f mediaFields
	if json.Unmarshal([]byte(line), &f) != nil {
		return mediaTags{}
	}
	return f.tags()
}

func (f mediaFields) tags() mediaTags {
	year := field(f.ReleaseYear)
	for _, date := range []any{f.ReleaseDate, f.UploadDate} {
		if d := field(date); year == "" && len(d) >= 4 {
			year = d[:4]
		}
	}
	return mediaTags{
		Title:       first(f.Track, f.Title),
		Artist:      first(f.Artist, f.Artists, f.Creator, f.Uploader, f.Channel),
		AlbumArtist: field(f.AlbumArtist),
		Album:       field(f.Album),
		Track:       field(f.TrackNumber),
		Date:        year,
		Comment:     field(f.WebpageURL),
	}
}

// first is the first of fields that is known.
func first(fields ...any) string {
	for _, f := range fields {
		if text := field(f); text != "" {
			return text
		}
	}
	return ""
}

// field is a field yt-dlp wrote as text: a string as it is, a number in
// digits, a list joined with commas, and nothing for null, NA or a line
// break, which no tag holds.
func field(v any) string {
	var text string
	switch v := v.(type) {
	case string:
		text = v
	case float64:
		text = strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		var parts []string
		for _, part := range v {
			if p := field(part); p != "" {
				parts = append(parts, p)
			}
		}
		text = strings.Join(parts, ", ")
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "NA" {
		return ""
	}
	return text
}

// args are the -metadata arguments of ffmpeg for the tags.
func (t mediaTags) args() []string {
	var args []string
	for _, tag := range []struct{ key, value string }{
		{"title", t.Title}, {"artist", t.Artist}, {"album_artist", t.AlbumArtist}, {"album", t.Album},
		{"track", t.Track}, {"date", t.Date}, {"comment", t.Comment},
	} {
		if tag.value != "" {
			args = append(args, "-metadata", tag.key+"="+tag.value)
		}
	}
	return args
}

// findCover is the thumbnail yt-dlp wrote to meta/, or "" where it wrote
// none.
func findCover(work string) string {
	entries, err := os.ReadDir(filepath.Join(work, mediaMeta))
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, "cover.") || strings.HasSuffix(name, ".part") {
			continue
		}
		if info, err := entry.Info(); err == nil && info.Size() > 0 {
			return filepath.Join(work, mediaMeta, name)
		}
	}
	return ""
}

// mp3Args are the arguments ffmpeg is run with to make an MP3 at out of the
// audio in, at the highest VBR quality or, where copy says in is an MP3
// already, as it is, tagged with tags and with the middle square of cover as
// its front cover where there is a cover. It says its progress on stdout.
func mp3Args(in, cover, out string, tags mediaTags, copy bool) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-y", "-i", "file:" + in}
	if cover != "" {
		args = append(args, "-i", "file:"+cover)
	}
	args = append(args, "-map", "0:a:0")
	if cover != "" {
		args = append(args, "-map", "1:v:0", "-c:v", "mjpeg", "-q:v", "2",
			"-vf", "crop='min(iw,ih)':'min(iw,ih)'", "-disposition:v:0", "attached_pic",
			"-metadata:s:v", "title=Album cover", "-metadata:s:v", "comment=Cover (front)")
	}
	if copy {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "libmp3lame", "-q:a", "0")
	}
	args = append(args, "-map_metadata", "-1", "-id3v2_version", "3")
	args = append(args, tags.args()...)
	return append(args, "-f", "mp3", "-progress", "pipe:1", "file:"+out)
}

// toMP3 makes an MP3 of the audio yt-dlp downloaded to staged, tagged with
// what it left in meta/ of work, and says where that is. An MP3 is copied
// rather than converted, which takes seconds, for its tags. A cover ffmpeg
// cannot take does not cost the fetch its MP3: it is made again without.
func (m *mediaRun) toMP3(ctx context.Context, program, work, staged string) (string, error) {
	ext := filepath.Ext(staged)
	copy := strings.EqualFold(ext, ".mp3")
	target := strings.TrimSuffix(staged, ext) + ".mp3"
	// mediaFile passes over a .part, so the MP3 counts only once it is whole
	part := target + ".part"
	tags := readMediaTags(work)
	cover := findCover(work)
	err := m.ffmpeg(ctx, program, mp3Args(staged, cover, part, tags, copy))
	if err != nil && cover != "" && ctx.Err() == nil {
		if m.log != nil {
			m.log.Warn("http fetch cover left out", "cover", filepath.Base(cover), "error", err)
		}
		err = m.ffmpeg(ctx, program, mp3Args(staged, "", part, tags, copy))
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(part, target); err != nil {
		return "", err
	}
	if target != staged {
		_ = os.Remove(staged)
	}
	return target, nil
}

// ffmpeg runs ffmpeg with args to its end, showing its progress in the job,
// and fails with what it said went wrong.
func (m *mediaRun) ffmpeg(ctx context.Context, program string, args []string) error {
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
		return fmt.Errorf("ffmpeg cannot be started: %w", err)
	}
	m.job.converting()
	conv := &mediaConversion{job: m.job, input: -1}
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
			return fmt.Errorf("ffmpeg failed with exit status %d: %s", exit.ExitCode(), last)
		case errors.As(err, &exit):
			return fmt.Errorf("ffmpeg failed with exit status %d", exit.ExitCode())
		default:
			return fmt.Errorf("ffmpeg failed: %w", err)
		}
	}
	return nil
}

// mediaConversion follows one run of ffmpeg: it reads the length of the
// audio from what ffmpeg says on stderr, and how far it is from its progress
// on stdout, into the job.
type mediaConversion struct {
	job   *registryJob
	timer *time.Timer

	mu sync.Mutex
	// input is the input of ffmpeg whose description it prints just now,
	// -1 before the first: the audio is input 0, and a cover input 1.
	input int
	// total and done are the length of the audio and how much of it is
	// converted, in microseconds; speed is how many times faster than it
	// plays ffmpeg converts.
	total, done int64
	speed       float64
	// stderr is the end of what ffmpeg said there.
	stderr []byte
}

// said takes a line ffmpeg printed on stderr: the Duration below Input #0
// is that of the audio.
func (c *mediaConversion) said(line string) {
	line = strings.TrimRight(line, "\r")
	c.mu.Lock()
	defer c.mu.Unlock()
	if rest, ok := strings.CutPrefix(line, "Input #"); ok {
		number, _, _ := strings.Cut(rest, ",")
		if n, err := strconv.Atoi(number); err == nil {
			c.input = n
		}
	}
	if line != "" && line[0] != ' ' && !strings.HasPrefix(line, "Input #") {
		// the description of the inputs is indented under their line
		c.input = -1
	}
	if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Duration: "); ok && c.input == 0 && c.total == 0 {
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
