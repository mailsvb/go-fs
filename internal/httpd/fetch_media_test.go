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

func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(fakeYtDlpMode); ok {
		os.Exit(fakeYtDlp(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
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
	}

	dir := value("-P")
	ext := "mp4"
	switch {
	case value("--audio-format") == "mp3":
		ext = "mp3"
	case strings.HasPrefix(value("-f"), "ba"):
		ext = "m4a"
	}
	// the video and then the audio, each counting from zero
	for _, line := range []string{"0|1000|NA", "600|1000|NA", "1000|1000|NA", "0|NA|500.5", "500|NA|500.5"} {
		fmt.Println(mediaProgress + line + "|A Song | Live")
	}
	if has("--merge-output-format") || has("-x") {
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
// and which has an ffmpeg where withFFmpeg says so.
func mediaServer(t *testing.T, mode string, withFFmpeg bool, tune func(*httpConfig)) *testServer {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeYtDlpMode, mode)
	return newServer(t, func(cfg *httpConfig) {
		cfg.YtDlp = config.Tool{Path: self, Version: "2026.10.31"}
		if withFFmpeg {
			cfg.FFmpeg = config.Tool{Path: "/opt/ffmpeg/bin/ffmpeg", Version: "7.1"}
		}
		cfg.UploadStagingFolder = t.TempDir()
		if tune != nil {
			tune(cfg)
		}
	})
}

const youTubeURL = "https://www.youtube.com/watch?v=abc123"

func TestMediaFetchStoresWhatYtDlpMade(t *testing.T) {
	for _, test := range []struct {
		media      string
		withFFmpeg bool
		file       string
		args       []string
		notArgs    []string
	}{
		{mediaVideo, true, "A Song [abc123].mp4",
			[]string{"bv*+ba/b", "--merge-output-format", "--ffmpeg-location\n/opt/ffmpeg/bin/ffmpeg"}, nil},
		{mediaVideo, false, "A Song [abc123].mp4", []string{"-f\nb\n"}, []string{"--ffmpeg-location"}},
		{mediaAudio, true, "A Song [abc123].mp3", []string{"-x", "--audio-format\nmp3"}, nil},
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
