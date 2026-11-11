package logging

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestURLMasksCredentials(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/rest/ping.view?u=john&p=secret&v=1.16.1", "/rest/ping.view?u=john&p=***&v=1.16.1"},
		{"/rest/ping?t=abc&s=salt&apiKey=k", "/rest/ping?t=***&s=***&apiKey=***"},
		{"/Users?api_key=a&ApiKey=b", "/Users?api_key=***&ApiKey=***"},
		{"/f.txt?key=share&view=1", "/f.txt?key=***&view=1"},
		{"/b/o?X-Amz-Signature=sig&X-Amz-Date=now", "/b/o?X-Amz-Signature=***&X-Amz-Date=now"},
		{"/a?refresh", "/a?refresh"},
		{"/a?access%5Ftoken=x", "/a?access%5Ftoken=***"},
		{"https://bob:pw@host/x?session_token=t#frag", "https://bob:%2A%2A%2A@host/x?session_token=***"},
		{"https://bob@host/x", "https://bob@host/x"},
		{"/plain/path", "/plain/path"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := URL(u); got != tc.want {
			t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := URLString("%zz"); got != Masked {
		t.Errorf("an unparsable URL is shown as %q", got)
	}
}

func TestHeadersMaskCredentials(t *testing.T) {
	h := http.Header{
		"Authorization":        {"Basic am9objpkb2U="},
		"Cookie":               {"goFsSessionToken=abc; theme=dark"},
		"Set-Cookie":           {"goFsSessionToken=abc; Path=/; HttpOnly"},
		"X-Emby-Authorization": {`MediaBrowser Client="Infuse", Device="tv", Token="jellyfintoken"`},
		"X-Emby-Token":         {"embytoken"},
		"X-Api-Key":            {"apikey"},
		"Location":             {"https://cdn.example/blob?X-Amz-Signature=sig&a=1"},
		"User-Agent":           {"curl/8"},
	}
	var out bytes.Buffer
	slog.New(slog.NewTextHandler(&out, nil)).Info("x", "header", Headers(h))
	logged := out.String()
	for _, secret := range []string{"am9objpkb2U", "abc", "jellyfintoken", "embytoken", "apikey", "=sig"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the header record holds %q: %s", secret, logged)
		}
	}
	for _, kept := range []string{"Basic ***", "goFsSessionToken=***", "theme=***", "Path=/; HttpOnly",
		`Client=\"Infuse\"`, `Token=***`, "curl/8", "a=1"} {
		if !strings.Contains(logged, kept) {
			t.Errorf("the header record lacks %q: %s", kept, logged)
		}
	}

	long := http.Header{"X-Long": {strings.Repeat("a", 2*maxHeaderValue)}}
	var cut bytes.Buffer
	slog.New(slog.NewTextHandler(&cut, nil)).Info("x", "header", Headers(long))
	if cut.Len() > maxHeaderValue+200 {
		t.Errorf("a long header is not cut: %d bytes", cut.Len())
	}
}

func TestArgsMaskCredentials(t *testing.T) {
	got := Args([]string{
		"--password", "pw1", "--video-password=pw2", "-s", "1280x720",
		"https://www.example.com/watch?v=x&token=tk", "--output", "/tmp/a",
	})
	want := []string{
		"--password", Masked, "--video-password=" + Masked, "-s", "1280x720",
		"https://www.example.com/watch?v=x&token=***", "--output", "/tmp/a",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %q, want %q", got, want)
	}
}

func TestSecretIsMasked(t *testing.T) {
	var out bytes.Buffer
	slog.New(slog.NewJSONHandler(&out, nil)).Info("x", "password", Secret("hunter2"))
	if strings.Contains(out.String(), "hunter2") || !strings.Contains(out.String(), Masked) {
		t.Errorf("record = %s", out.String())
	}
}
