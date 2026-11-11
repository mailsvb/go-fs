package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// records parses the JSON lines a test logger wrote.
func records(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var all []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		all = append(all, record)
	}
	return all
}

func here() string {
	_, file, line, _ := runtime.Caller(1)
	return file[strings.LastIndex(file, "/")+1:] + ":" + strconv.Itoa(line+1)
}

// A record written through a helper names the line of the helper's caller,
// which is the one that tells two file accesses apart.
func TestHelpersNameTheirCaller(t *testing.T) {
	var out bytes.Buffer
	log := NewTo(&out, "debug", "json").Logger
	want := here()
	FileOp(log, "open", "/srv/a.txt", os.ErrNotExist, slog.String("flags", "r"))
	wantSyscall := here()
	Syscall(log, "fallocate", nil)

	got := records(t, &out)
	if len(got) != 2 {
		t.Fatalf("records = %v", got)
	}
	if got[0]["msg"] != "file open" || got[0]["path"] != "/srv/a.txt" || got[0]["flags"] != "r" ||
		got[0]["error"] == nil || got[0]["source"] != want {
		t.Errorf("file record = %v, want source %s", got[0], want)
	}
	if got[1]["msg"] != "syscall fallocate" || got[1]["source"] != wantSyscall || got[1]["error"] != nil {
		t.Errorf("syscall record = %v, want source %s", got[1], wantSyscall)
	}
}

func TestHelpersAreSilentAboveDebug(t *testing.T) {
	var out bytes.Buffer
	log := NewTo(&out, "info", "json").Logger
	FileOp(log, "open", "/a", nil)
	Syscall(log, "statfs", nil)
	FileOp(nil, "open", "/a", nil)
	if out.Len() != 0 {
		t.Errorf("records at info: %s", out.String())
	}
}

func TestFromFallsBack(t *testing.T) {
	fallback := Discard
	if From(context.Background(), fallback) != fallback {
		t.Error("a context without a logger did not give the fallback")
	}
	scoped := Discard.With("req", 1)
	if From(WithLogger(context.Background(), scoped), fallback) != scoped {
		t.Error("the logger of the context was not given")
	}
}

func TestTransportTracesWithoutSecrets(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final?X-Amz-Signature=signed", http.StatusFound)
			return
		}
		w.Header().Set("Set-Cookie", "session=cookievalue; Path=/")
		_, _ = io.WriteString(w, "body text")
	}))
	defer backend.Close()

	var out bytes.Buffer
	client := &http.Client{Transport: Transport(NewTo(&out, "debug", "json").Logger, "test", nil)}
	req, _ := http.NewRequest(http.MethodGet, backend.URL+"/redirect?token=querytoken", nil)
	req.Header.Set("Authorization", "Bearer bearertoken")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	logged := out.String()
	for _, secret := range []string{"querytoken", "bearertoken", "signed", "cookievalue", "body text"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the trace holds %q: %s", secret, logged)
		}
	}
	var sent, received int
	for _, record := range records(t, &out) {
		switch record["msg"] {
		case "out http >":
			sent++
		case "out http <":
			received++
			if record["peer"] != "test" || record["status"] == nil {
				t.Errorf("answer record = %v", record)
			}
		}
	}
	if sent != 2 || received != 2 {
		t.Errorf("%d requests and %d answers traced, want 2 of each (the redirect and its target)", sent, received)
	}
}

func TestConnCountsBytes(t *testing.T) {
	var out bytes.Buffer
	client, server := net.Pipe()
	traced := Conn(NewTo(&out, "debug", "json").Logger, "test", client)
	go func() {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(server, buf)
		_, _ = server.Write([]byte("abc"))
		server.Close()
	}()
	_, _ = traced.Write([]byte("hello"))
	buf := make([]byte, 3)
	_, _ = io.ReadFull(traced, buf)
	_ = traced.Close()
	_ = traced.Close()

	got := records(t, &out)
	if len(got) != 2 || got[0]["msg"] != "conn open" || got[1]["msg"] != "conn closed" {
		t.Fatalf("records = %v", got)
	}
	if got[1]["bytesOut"] != float64(5) || got[1]["bytesIn"] != float64(3) {
		t.Errorf("closed record = %v", got[1])
	}
}

// TestMain lets the test binary stand in for a program that a test of
// Output runs.
func TestMain(m *testing.M) {
	if os.Getenv("GOFS_LOGGING_HELPER") == "1" {
		_, _ = io.WriteString(os.Stdout, "said on stdout\n")
		_, _ = io.WriteString(os.Stderr, "said on stderr\n")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func TestOutputRecordsTheProgram(t *testing.T) {
	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=none", "--password", "programpassword")
	cmd.Env = append(os.Environ(), "GOFS_LOGGING_HELPER=1")
	want := here()
	stdout, err := Output(NewTo(&out, "debug", "json").Logger, cmd)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(string(exit.Stderr), "said on stderr") {
		t.Fatalf("err = %v", err)
	}
	if string(stdout) != "said on stdout\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if strings.Contains(out.String(), "programpassword") {
		t.Errorf("the record holds the password: %s", out.String())
	}
	got := records(t, &out)
	if len(got) != 2 || got[0]["msg"] != "exec start" || got[1]["msg"] != "exec exit" {
		t.Fatalf("records = %v", got)
	}
	if got[0]["source"] != want || got[0]["pid"] == nil {
		t.Errorf("start record = %v, want source %s", got[0], want)
	}
	if got[1]["exitCode"] != float64(3) || got[1]["stderr"] != "said on stderr" {
		t.Errorf("exit record = %v", got[1])
	}
}
