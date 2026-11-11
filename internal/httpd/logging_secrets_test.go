package httpd

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The debug trace of a request names its URL and its header, and either may
// carry what authenticates it: a Subsonic password, a Jellyfin api_key, the
// key of a share link, an S3 signature, a Basic login, the session cookie.
// None of those may reach the log.
func TestTheRequestTraceMasksCredentials(t *testing.T) {
	server := newServer(t, nil)
	secrets := []string{
		"subsonicpw1", "subsonictoken2", "subsonicsalt3", "subsonicapikey4",
		"jellyfinkey5", "sharekey6", "amzsignature7", "amzcredential8", "sonosplay9",
		"am9objpkb2U", "sessioncookie10", "embytoken11",
	}
	paths := []string{
		"/rest/ping.view?u=john&p=subsonicpw1&t=subsonictoken2&s=subsonicsalt3&apiKey=subsonicapikey4&v=1.16.1&c=test",
		"/Users/Me?api_key=jellyfinkey5",
		"/docs/report.txt?key=sharekey6",
		"/bucket/object?X-Amz-Signature=amzsignature7&X-Amz-Credential=amzcredential8&X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"/music/song.mp3?go-fs-play=sonosplay9",
	}
	for _, path := range paths {
		req, err := http.NewRequest(http.MethodGet, server.url(path), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Basic am9objpkb2U=")
		req.Header.Set("Cookie", "goFsSessionToken=sessioncookie10")
		req.Header.Set("X-Emby-Token", "embytoken11")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	server.logs.mu.Lock()
	defer server.logs.mu.Unlock()
	traced := 0
	for _, record := range server.logs.records {
		// fmt, not JSON, so that a group such as the header is written out
		// as well: an slog.Value has no JSON encoding of its own
		logged := fmt.Sprint(record)
		for _, secret := range secrets {
			if strings.Contains(logged, secret) {
				t.Errorf("a record holds %q: %s", secret, logged)
			}
		}
		if record["msg"] == "http <" {
			traced++
			if !strings.Contains(logged, "Basic ***") {
				t.Errorf("the request record does not show the header masked: %s", logged)
			}
		}
	}
	if traced < len(paths) {
		t.Errorf("%d of %d requests were traced", traced, len(paths))
	}
}

// A request is traced as "http <" and "http >", and every record written
// while it is served carries the same req, the files it opened among them,
// which is what ties them together.
func TestTheRequestTraceTiesRecordsTogether(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "docs/report.txt", "the report")
	res := basic(t, server, http.MethodGet, "/docs/report.txt", "john", "doe", nil)
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()

	server.logs.mu.Lock()
	defer server.logs.mu.Unlock()
	var in, out, authenticated, opened map[string]any
	for _, record := range server.logs.records {
		switch record["msg"] {
		case "http <":
			in = record
		case "http >":
			out = record
		case "http authenticated":
			authenticated = record
		case "file open":
			if strings.HasSuffix(fmt.Sprint(record["path"]), "report.txt") {
				opened = record
			}
		}
	}
	if in == nil || out == nil || authenticated == nil || opened == nil {
		t.Fatalf("records missing: in %v, out %v, authenticated %v, file open %v", in, out, authenticated, opened)
	}
	if in["req"] == nil || in["req"] != out["req"] || in["req"] != authenticated["req"] || in["req"] != opened["req"] {
		t.Errorf("the records do not share their req: %v, %v, %v, %v",
			in["req"], out["req"], authenticated["req"], opened["req"])
	}
	if out["status"] != int64(http.StatusOK) || out["bytes"] != int64(len("the report")) {
		t.Errorf("answer record = %v", out)
	}
	if header := fmt.Sprint(out["header"]); !strings.Contains(header, "Content-Type") {
		t.Errorf("the answer record does not show the header: %s", header)
	}
}
