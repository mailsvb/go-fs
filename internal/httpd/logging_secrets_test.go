package httpd

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The debug trace of a request names its URL, and a URL may carry what
// authenticates it: a Subsonic password, a Jellyfin api_key, the key of a
// share link, an S3 signature. None of those may reach the log.
func TestTheRequestTraceMasksCredentials(t *testing.T) {
	server := newServer(t, nil)
	secrets := []string{
		"subsonicpw1", "subsonictoken2", "subsonicsalt3", "subsonicapikey4",
		"jellyfinkey5", "sharekey6", "amzsignature7", "amzcredential8", "sonosplay9",
	}
	paths := []string{
		"/rest/ping.view?u=john&p=subsonicpw1&t=subsonictoken2&s=subsonicsalt3&apiKey=subsonicapikey4&v=1.16.1&c=test",
		"/Users/Me?api_key=jellyfinkey5",
		"/docs/report.txt?key=sharekey6",
		"/bucket/object?X-Amz-Signature=amzsignature7&X-Amz-Credential=amzcredential8&X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"/music/song.mp3?go-fs-play=sonosplay9",
	}
	for _, path := range paths {
		resp, err := http.Get(server.url(path))
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
		logged, _ := json.Marshal(record)
		for _, secret := range secrets {
			if strings.Contains(string(logged), secret) {
				t.Errorf("a record holds %q: %s", secret, logged)
			}
		}
		if record["msg"] == "http request" {
			traced++
		}
	}
	if traced < len(paths) {
		t.Errorf("%d of %d requests were traced", traced, len(paths))
	}
}
