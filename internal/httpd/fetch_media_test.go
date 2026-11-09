package httpd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
)

// fakeYtDlpMode is set to make the test binary act as yt-dlp, which a test
// cannot count on being installed: the server runs the binary it was given as
// yt-dlp, and here that is this one.
const fakeYtDlpMode = "GOFS_FAKE_YTDLP"

// fakeFFmpegMode is how the test binary acts as ffmpeg, which it is too: the
// server runs ffmpeg with -progress, and yt-dlp never.
const fakeFFmpegMode = "GOFS_FAKE_FFMPEG"

func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(fakeYtDlpMode); ok {
		for _, arg := range os.Args[1:] {
			if arg == "-progress" {
				os.Exit(fakeFFmpeg(os.Getenv(fakeFFmpegMode), os.Args[1:]))
			}
		}
		os.Exit(fakeYtDlp(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeFFmpeg does what ffmpeg would when the server converts an audio to an
// MP3: it says the length of the input on stderr and its progress on stdout,
// and writes the output, whose content is the input's and the arguments it
// was run with.
func fakeFFmpeg(mode string, args []string) int {
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "Input #0, matroska,webm, from 'in.webm':")
		fmt.Fprintln(os.Stderr, "file:in.webm: Invalid data found when processing input")
		return 1
	}
	fmt.Fprintln(os.Stderr, "Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'in.m4a':")
	fmt.Fprintln(os.Stderr, "  Duration: 00:01:40.00, start: 0.000000, bitrate: 128 kb/s")
	if mode == "hang" {
		// ffmpeg says where it is twice a second, however slow it goes
		for range 600 {
			fmt.Print("out_time_us=25000000\nspeed=20.0x\nprogress=continue\n")
			time.Sleep(100 * time.Millisecond)
		}
		return 0
	}
	fmt.Print("out_time_us=N/A\nspeed=N/A\nprogress=continue\n")
	fmt.Print("out_time_us=50000000\nspeed=20.0x\nprogress=continue\n")
	fmt.Print("out_time_us=100000000\nspeed= 20x\nprogress=end\n")
	var in string
	for i, arg := range args {
		if arg == "-i" && i+1 < len(args) {
			in = strings.TrimPrefix(args[i+1], "file:")
		}
	}
	out := strings.TrimPrefix(args[len(args)-1], "file:")
	content, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	content = append(content, "\n--ffmpeg--\n"+strings.Join(args, "\n")...)
	if err := os.WriteFile(out, content, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// fakeYtDlp does what yt-dlp would, as far as the server can tell: it prints
// its progress as the template asks and leaves one file in the folder of -P,
// named by -o, whose content is the arguments it was run with.
func fakeYtDlp(mode string, args []string) int {
	value := func(flag string) string {
		for i, arg := range args {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	has := func(flag string) bool {
		for _, arg := range args {
			if arg == flag {
				return true
			}
		}
		return false
	}
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] abc123: Video unavailable")
		return 1
	case "crash":
		fmt.Fprintln(os.Stderr, "Traceback: something broke")
		return 2
	case "large":
		fmt.Println("[download] File is larger than max-filesize (2000 bytes > 10 bytes). Aborting.")
		return 0
	case "hang":
		fmt.Println(mediaProgress + "100|1000|NA|Slow")
		time.Sleep(time.Minute)
		return 0
	case "merging":
		fmt.Println(mediaProgress + "0|1000|NA|Long Film")
		fmt.Println(mediaProgress + "1000|1000|NA|Long Film")
		fmt.Println("[Merger] Merging formats into \"" + filepath.Join(value("-P"), "Long Film [abc123].mp4") + "\"")
		time.Sleep(time.Minute)
		return 0
	}

	dir := value("-P")
	ext := "mp4"
	switch {
	case mode == "mp3":
		// a site that sends its audio as an MP3
		ext = "mp3"
	case strings.HasPrefix(value("-f"), "ba"):
		ext = "m4a"
	}
	// the video and then the audio, each counting from zero
	for _, line := range []string{"0|1000|NA", "600|1000|NA", "1000|1000|NA", "0|NA|500.5", "500|NA|500.5"} {
		fmt.Println(mediaProgress + line + "|A Song | Live")
	}
	if has("--merge-output-format") {
		fmt.Println("[Merger] Merging formats into \"" + filepath.Join(dir, "A Song [abc123]."+ext) + "\"")
	}
	name := "A Song [abc123]." + ext
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(args, "\n")), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	if mode == "two" {
		_ = os.WriteFile(filepath.Join(dir, "other.mp4"), []byte("x"), 0o600)
	}
	return 0
}

// mediaServer is a server whose yt-dlp is this test binary, acting as mode,
// and which has an ffmpeg where withFFmpeg says so, this binary again, acting
// as $GOFS_FAKE_FFMPEG says.
func mediaServer(t *testing.T, mode string, withFFmpeg bool, tune func(*httpConfig)) *testServer {
	t.Helper()
	self := testBinary(t)
	t.Setenv(fakeYtDlpMode, mode)
	if _, ok := os.LookupEnv(fakeFFmpegMode); !ok {
		t.Setenv(fakeFFmpegMode, "ok")
	}
	return newServer(t, func(cfg *httpConfig) {
		cfg.YtDlp = config.Tool{Path: self, Version: "2026.10.31"}
		if withFFmpeg {
			cfg.FFmpeg = config.Tool{Path: self, Version: "7.1"}
		}
		cfg.UploadStagingFolder = t.TempDir()
		if tune != nil {
			tune(cfg)
		}
	})
}

const youTubeURL = "https://www.youtube.com/watch?v=abc123"

func testBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func TestMediaFetchStoresWhatYtDlpMade(t *testing.T) {
	for _, test := range []struct {
		media      string
		withFFmpeg bool
		file       string
		args       []string
		notArgs    []string
	}{
		{mediaVideo, true, "A Song [abc123].mp4",
			[]string{"bv*+ba/b", "--merge-output-format", "--ffmpeg-location\n" + testBinary(t)}, []string{"--ffmpeg--"}},
		{mediaVideo, false, "A Song [abc123].mp4", []string{"-f\nb\n"}, []string{"--ffmpeg-location"}},
		// yt-dlp downloads the audio as it is, and go-fs has ffmpeg convert it
		{mediaAudio, true, "A Song [abc123].mp3",
			[]string{"-f\nba/b\n", "--ffmpeg--", "libmp3lame", "-q:a\n0", "-progress\npipe:1", ".m4a\n", ".mp3.part"},
			[]string{"-x\n", "--audio-format"}},
		{mediaAudio, false, "A Song [abc123].m4a", []string{"ba[ext=m4a]/ba"}, []string{"-x", "--ffmpeg-location"}},
	} {
		t.Run(fmt.Sprintf("%s ffmpeg=%v", test.media, test.withFFmpeg), func(t *testing.T) {
			server := mediaServer(t, "ok", test.withFFmpeg, nil)
			session := login(t, server, "/", "john", "doe")
			job := fetched(t, server, "/", session, fetchBody{URL: youTubeURL, Media: test.media})
			expectDone(t, job)
			if job.Name != test.file || !strings.Contains(job.Message, test.file) {
				t.Errorf("the job names %q and says %q", job.Name, job.Message)
			}
			args := server.read(t, test.file)
			for _, want := range append(test.args, "--ignore-config", "--no-playlist", "--\n"+youTubeURL) {
				if !strings.Contains(args, want) {
					t.Errorf("yt-dlp was not given %q:\n%s", want, args)
				}
			}
			for _, unwanted := range test.notArgs {
				if strings.Contains(args, unwanted) {
					t.Errorf("yt-dlp was given %q:\n%s", unwanted, args)
				}
			}
			if job.BytesDone != int64(len(args)) || job.BytesTotal != job.BytesDone {
				t.Errorf("the job counts %d of %d bytes, want %d", job.BytesDone, job.BytesTotal, len(args))
			}
		})
	}
}

func TestMediaFetchSaysWhyYtDlpFailed(t *testing.T) {
	for mode, says := range map[string]string{
		"fail":  "[youtube] abc123: Video unavailable",
		"crash": "exit status 2: Traceback: something broke",
		"two":   "stored 2 files",
	} {
		server := mediaServer(t, mode, true, nil)
		session := login(t, server, "/", "john", "doe")
		expectFailed(t, fetched(t, server, "/", session, fetchBody{URL: youTubeURL, Media: mediaVideo}), says)
		if entries, _ := os.ReadDir(server.base); len(entries) != 0 {
			t.Errorf("%s: the folder holds %v", mode, entries)
		}
	}
}

func TestMediaFetchIsRefusedWhatItCannotDo(t *testing.T) {
	without := newServer(t, nil)
	session := login(t, without, "/", "john", "doe")
	res, data := transferRequest(t, without, http.MethodPost, "/?go-fs=fetch", session,
		fetchBody{URL: youTubeURL, Media: mediaVideo})
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "needs yt-dlp") {
		t.Errorf("without yt-dlp a video fetch answered %d: %s", res.StatusCode, data)
	}

	server := mediaServer(t, "ok", true, nil)
	session = login(t, server, "/", "john", "doe")
	for _, body := range []fetchBody{
		{URL: youTubeURL, Media: mediaAudio, Username: "me"},
		{URL: youTubeURL, Media: mediaAudio, Password: "secret"},
		{URL: youTubeURL, Media: mediaVideo, Headers: "Cookie: a=b"},
		{URL: youTubeURL, Media: "subtitles"},
		{URL: "ftp://example.com/x", Media: mediaVideo},
	} {
		res, data := transferRequest(t, server, http.MethodPost, "/?go-fs=fetch", session, body)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%+v answered %d: %s", body, res.StatusCode, data)
		}
	}
}

func TestMediaFetchKeepsToTheRulesOfAFetch(t *testing.T) {
	server := mediaServer(t, "ok", true, func(cfg *httpConfig) {
		kim := fullUser("kim", "pw")
		kim.AllowUserFileOverwrite = new(false)
		cfg.Users = append(cfg.Users, kim)
	})
	server.write(t, "A Song [abc123].mp4", "old")
	kim := login(t, server, "/", "kim", "pw")
	expectFailed(t, fetched(t, server, "/", kim, fetchBody{URL: youTubeURL, Media: mediaVideo}), "already a file")
	if got := server.read(t, "A Song [abc123].mp4"); got != "old" {
		t.Errorf("the file was replaced: %q", got)
	}
	john := login(t, server, "/", "john", "doe")
	expectDone(t, fetched(t, server, "/", john, fetchBody{URL: youTubeURL, Media: mediaVideo}))
	if got := server.read(t, "A Song [abc123].mp4"); got == "old" {
		t.Error("the file was not replaced")
	}
}

func TestMediaFetchKeepsToTheUploadLimit(t *testing.T) {
	for _, mode := range []string{"large", "ok"} {
		var staging string
		server := mediaServer(t, mode, true, func(cfg *httpConfig) {
			cfg.MaxUploadSize = 10
			staging = cfg.UploadStagingFolder
		})
		session := login(t, server, "/", "john", "doe")
		expectFailed(t, fetched(t, server, "/", session, fetchBody{URL: youTubeURL, Media: mediaVideo}),
			"larger than the maximum of 10 bytes")
		if entries, _ := os.ReadDir(server.base); len(entries) != 0 {
			t.Errorf("%s: the folder holds %v", mode, entries)
		}
		expectNoWorkFolder(t, staging)
	}
}

func expectNoWorkFolder(t *testing.T, staging string) {
	t.Helper()
	entries, _ := os.ReadDir(staging)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "media-") {
			t.Errorf("the staging folder kept %s", entry.Name())
		}
	}
}

func TestMediaFetchCanBeStopped(t *testing.T) {
	var staging string
	server := mediaServer(t, "hang", true, func(cfg *httpConfig) {
		staging = cfg.UploadStagingFolder
	})
	session := login(t, server, "/", "john", "doe")
	job := startFetch(t, server, "/", session, fetchBody{URL: youTubeURL, Media: mediaVideo})
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, data := transferRequest(t, server, http.MethodGet, "/?go-fs=fetch-job&id="+job.ID, session, nil)
		var view registryJobJSON
		_ = json.Unmarshal(data, &view)
		if view.Phase == fetchDownloading {
			if view.BytesDone != 100 || view.BytesTotal != 1000 || view.Name != "Slow" {
				t.Errorf("the job is %+v", view)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the download did not begin: %+v", view)
		}
		time.Sleep(20 * time.Millisecond)
	}
	started := time.Now()
	res, data := transferRequest(t, server, http.MethodDelete, "/?go-fs=fetch-job&id="+job.ID, session, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("the stop answered %d: %s", res.StatusCode, data)
	}
	if ended := waitForFetch(t, server, "/", job.ID, session); ended.State != jobCancelled {
		t.Errorf("the fetch ended %s: %s", ended.State, ended.Message)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("stopping took %s", took)
	}
	expectNoWorkFolder(t, staging)
}

// convertingView waits for the job to be converting, with a length that
// ffmpeg read where withLength says so, and says what it is then. ffmpeg says
// the length on stderr and its progress on stdout, which are read apart, so
// the first progress can come before the length.
func convertingView(t *testing.T, server *testServer, session *http.Cookie, id string, withLength bool) registryJobJSON {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, data := transferRequest(t, server, http.MethodGet, "/?go-fs=fetch-job&id="+id, session, nil)
		var view registryJobJSON
		_ = json.Unmarshal(data, &view)
		if view.Phase == fetchConverting && (view.ConvertTotal > 0) == withLength {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("the conversion was not shown: %+v", view)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMediaFetchShowsTheConversion checks that while ffmpeg converts an
// audio into an MP3, the job says how far it is and how fast, rather than a
// speed of the download that falls towards nothing, and that Stop ends it.
func TestMediaFetchShowsTheConversion(t *testing.T) {
	t.Setenv(fakeFFmpegMode, "hang")
	var staging string
	server := mediaServer(t, "ok", true, func(cfg *httpConfig) {
		staging = cfg.UploadStagingFolder
	})
	session := login(t, server, "/", "john", "doe")
	job := startFetch(t, server, "/", session, fetchBody{URL: youTubeURL, Media: mediaAudio})
	view := convertingView(t, server, session, job.ID, true)
	if view.State != jobRunning || view.ConvertDone != 25000 || view.ConvertTotal != 100000 ||
		view.ConvertSpeed != 20 || view.BytesPerSecond != 0 || view.BytesDone == 0 || view.BytesDone != view.BytesTotal {
		t.Errorf("the job is %+v", view)
	}
	started := time.Now()
	transferRequest(t, server, http.MethodDelete, "/?go-fs=fetch-job&id="+job.ID, session, nil)
	if ended := waitForFetch(t, server, "/", job.ID, session); ended.State != jobCancelled {
		t.Errorf("the fetch ended %s: %s", ended.State, ended.Message)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("stopping took %s", took)
	}
	expectNoWorkFolder(t, staging)
}

// TestMediaFetchSaysItJoins checks that a video whose video and audio
// ffmpeg joins, which says nothing of how far it is, is shown as converting
// with no progress of its own.
func TestMediaFetchSaysItJoins(t *testing.T) {
	server := mediaServer(t, "merging", true, nil)
	session := login(t, server, "/", "john", "doe")
	job := startFetch(t, server, "/", session, fetchBody{URL: youTubeURL, Media: mediaVideo})
	view := convertingView(t, server, session, job.ID, false)
	if view.State != jobRunning || view.BytesDone != 1000 || view.BytesTotal != 1000 ||
		view.BytesPerSecond != 0 || view.ConvertTotal != 0 || view.Name != "Long Film" {
		t.Errorf("the job is %+v", view)
	}
	transferRequest(t, server, http.MethodDelete, "/?go-fs=fetch-job&id="+job.ID, session, nil)
	if ended := waitForFetch(t, server, "/", job.ID, session); ended.State != jobCancelled {
		t.Errorf("the fetch ended %s: %s", ended.State, ended.Message)
	}
}

func TestMediaFetchSaysWhyFFmpegFailed(t *testing.T) {
	t.Setenv(fakeFFmpegMode, "fail")
	server := mediaServer(t, "ok", true, nil)
	session := login(t, server, "/", "john", "doe")
	expectFailed(t, fetched(t, server, "/", session, fetchBody{URL: youTubeURL, Media: mediaAudio}),
		"ffmpeg failed with exit status 1: file:in.webm: Invalid data found when processing input")
	if entries, _ := os.ReadDir(server.base); len(entries) != 0 {
		t.Errorf("the failed fetch left %d files", len(entries))
	}
}

// TestMediaFetchTakesAnMP3AsItIs checks that an audio the site sends as an
// MP3 is not converted again: an ffmpeg that fails is never run.
func TestMediaFetchTakesAnMP3AsItIs(t *testing.T) {
	t.Setenv(fakeFFmpegMode, "fail")
	server := mediaServer(t, "mp3", true, nil)
	session := login(t, server, "/", "john", "doe")
	job := fetched(t, server, "/", session, fetchBody{URL: youTubeURL, Media: mediaAudio})
	expectDone(t, job)
	if content := server.read(t, "A Song [abc123].mp3"); strings.Contains(content, "--ffmpeg--") {
		t.Errorf("the MP3 was converted:\n%s", content)
	}
}

func TestMediaConversionReadsFFmpeg(t *testing.T) {
	job := &registryJob{}
	conv := &mediaConversion{job: job, timer: time.AfterFunc(time.Hour, func() {})}
	defer conv.timer.Stop()
	for _, line := range []string{"Input #0, matroska,webm, from 'x.webm':",
		"  Duration: 01:02:03.50, start: -0.007000, bitrate: 131 kb/s",
		"Output #0, mp3, to 'x.mp3.part':", "  Duration: 00:00:01.00, start: 0.000000"} {
		conv.said(line)
	}
	for _, line := range []string{"out_time_us=N/A", "speed=N/A", "progress=continue"} {
		conv.progress(line)
	}
	if job.convertDone != 0 || job.convertTotal != 3723500000 || job.convertSpeed != 0 {
		t.Errorf("at first the job has %d of %d at %v", job.convertDone, job.convertTotal, job.convertSpeed)
	}
	for _, line := range []string{"bitrate= 245.1kbits/s", "out_time_us=1800000000\r", "speed= 31.5x", "progress=continue"} {
		conv.progress(line)
	}
	if job.convertDone != 1800000000 || job.convertSpeed != 31.5 {
		t.Errorf("the job has %d at %v", job.convertDone, job.convertSpeed)
	}
	// ffmpeg can go a little past the length it read
	for _, line := range []string{"out_time_us=3723600000", "progress=end"} {
		conv.progress(line)
	}
	if job.convertDone != job.convertTotal {
		t.Errorf("the end has %d of %d", job.convertDone, job.convertTotal)
	}
	for clock, want := range map[string]int64{"00:00:01.25": 1250000, "10:00:00.00": 36000000000, "N/A": 0, "1:2": 0} {
		if got := clockMicros(clock); got != want {
			t.Errorf("clockMicros(%q) = %d, want %d", clock, got, want)
		}
	}
}

func TestFetchOffersVideoWhileThereIsYtDlp(t *testing.T) {
	page := func(server *testServer) string {
		session := login(t, server, "/", "john", "doe")
		req, err := http.NewRequest(http.MethodGet, server.url("/"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(session)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return string(body)
	}
	if body := page(newServer(t, nil)); !strings.Contains(body, `id="fetch-dialog"`) || strings.Contains(body, `id="fetch-media"`) {
		t.Error("without yt-dlp the dialog offers video, or there is no dialog")
	}
	if body := page(mediaServer(t, "ok", false, nil)); !strings.Contains(body, `id="fetch-media"`) {
		t.Error("with yt-dlp the dialog offers no video")
	}
}

func TestMediaProgressCountsBothDownloads(t *testing.T) {
	job := &registryJob{}
	run := &mediaRun{job: job}
	for _, line := range []string{"0|1000|NA|T", "1000|1000|NA|T", "0|NA|500.5|T", "250|NA|500.5|T"} {
		run.progress(line)
	}
	if done, total := job.bytesDone.Load(), job.bytesTotal.Load(); done != 1250 || total != 1500 {
		t.Errorf("counted %d of %d, want 1250 of 1500", done, total)
	}
	if job.phase != fetchDownloading || job.name != "T" {
		t.Errorf("the job is at %q named %q", job.phase, job.name)
	}
}

func TestMediaJobName(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.youtube.com/watch?v=abc123&t=10": "abc123",
		"https://youtu.be/xyz789":                     "xyz789",
		"https://example.com/":                        "example.com",
	} {
		u, _ := url.Parse(raw)
		if got := mediaJobName(u); got != want {
			t.Errorf("%s is named %q, want %q", raw, got, want)
		}
	}
}

func TestIsYouTube(t *testing.T) {
	for host, want := range map[string]bool{
		"youtube.com":              true,
		"www.youtube.com":          true,
		"m.youtube.com":            true,
		"music.youtube.com":        true,
		"WWW.YouTube.COM":          true,
		"www.youtube.com.":         true,
		"youtu.be":                 true,
		"www.youtube-nocookie.com": true,
		"notyoutube.com":           false,
		"youtube.com.example.com":  false,
		"vimeo.com":                false,
		"127.0.0.1":                false,
		"":                         false,
	} {
		if got := isYouTube(host); got != want {
			t.Errorf("isYouTube(%q) = %v, want %v", host, got, want)
		}
	}
}

// YouTube asks yt-dlp to sign in over many IPv6 addresses, so its links are
// fetched over IPv4, and only its links.
func TestMediaArgsForceIPv4ForYouTube(t *testing.T) {
	set := &settings{cfg: config.HTTP{UploadStagingFolder: t.TempDir()}}
	for raw, want := range map[string]bool{
		"https://www.youtube.com/watch?v=jNQXAC9IVRw":     true,
		"https://m.youtube.com/watch?v=jNQXAC9IVRw":       true,
		"https://music.youtube.com/watch?v=jNQXAC9IVRw":   true,
		"https://youtu.be/jNQXAC9IVRw":                    true,
		"https://WWW.YOUTUBE.COM:443/watch?v=jNQXAC9IVRw": true,
		"https://vimeo.com/76979871":                      false,
		"https://notyoutube.com/watch?v=jNQXAC9IVRw":      false,
		"http://127.0.0.1:18182/x.mp4":                    false,
	} {
		u, _ := url.Parse(raw)
		args := mediaArgs(set, fetchRequest{url: u, media: mediaVideo}, t.TempDir())
		got := false
		for _, arg := range args {
			if arg == "--force-ipv4" {
				got = true
			}
		}
		if got != want {
			t.Errorf("%s: --force-ipv4 is passed %v, want %v", raw, got, want)
		}
		if args[len(args)-1] != u.String() || args[len(args)-2] != "--" {
			t.Errorf("%s: the URL is not last after --: %q", raw, args)
		}
	}
}
