package httpd

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// listenForTransfers opens the stream of events of a session and hands every
// transfer it tells of to the channel it returns.
func listenForTransfers(t *testing.T, server *testServer, session *http.Cookie) <-chan registryJobJSON {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.url("/?go-fs=events"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(session)
	res := do(t, req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", res.StatusCode)
	}
	if kind := res.Header.Get("Content-Type"); kind != "text/event-stream" {
		t.Fatalf("the stream is a %q", kind)
	}
	heard := make(chan registryJobJSON, 32)
	go func() {
		defer close(heard)
		scanner := bufio.NewScanner(res.Body)
		event := ""
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "transfer":
				var view registryJobJSON
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &view) == nil {
					heard <- view
				}
			case line == "":
				event = ""
			}
		}
	}()
	return heard
}

// hear waits for the stream to tell of a job in a state.
func hear(t *testing.T, heard <-chan registryJobJSON, id, state string) registryJobJSON {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case view, ok := <-heard:
			if !ok {
				t.Fatalf("the stream ended before the job was %s", state)
			}
			if view.ID == id && view.State == state {
				return view
			}
		case <-timeout:
			t.Fatalf("the stream did not tell of the job being %s", state)
		}
	}
}

func TestTheStreamTellsOfAFetchAsItStartsAndEnds(t *testing.T) {
	remote := origin(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pushed"))
	})
	server := newServer(t, nil)
	if err := os.Mkdir(server.base+"/in", 0o755); err != nil {
		t.Fatal(err)
	}
	session := login(t, server, "/", "john", "doe")
	// the stream is subscribed before its headers are sent
	heard := listenForTransfers(t, server, session)

	job := startFetch(t, server, "/in/", session, fetchBody{URL: remote.URL + "/files/told.txt"})
	hear(t, heard, job.ID, jobRunning)
	view := hear(t, heard, job.ID, jobDone)
	if view.Kind != jobFetch || view.Name != "told.txt" || view.Folder != "/in/" {
		t.Errorf("the stream told of a %s of %q into %q", view.Kind, view.Name, view.Folder)
	}
	if !strings.Contains(view.Message, "told.txt") {
		t.Errorf("the end says %q", view.Message)
	}
}

func TestTheStreamOutlivesTheTimeouts(t *testing.T) {
	remote := origin(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	})
	server := newServer(t, func(cfg *httpConfig) {
		cfg.ReadTimeout = 1
		cfg.WriteTimeout = 1
	})
	session := login(t, server, "/", "john", "doe")
	heard := listenForTransfers(t, server, session)
	job := startFetch(t, server, "/", session, fetchBody{URL: remote.URL + "/late.txt"})
	hear(t, heard, job.ID, jobDone)
}

func TestTheStreamIsASessionsOnly(t *testing.T) {
	server := newServer(t, nil)
	res, _ := transferRequest(t, server, http.MethodGet, "/?go-fs=events", nil, nil, "Authorization", "Basic "+
		basicToken("john", "doe"))
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("the stream answered %d to a Basic header", res.StatusCode)
	}
	session := login(t, server, "/", "john", "doe")
	res, _ = transferRequest(t, server, http.MethodPost, "/?go-fs=events", session, nil)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a POST to the stream answered %d", res.StatusCode)
	}
}

func TestTransferEventsReachTheirAccountOnly(t *testing.T) {
	var events transferEvents
	mine := events.subscribe("a")
	other := events.subscribe("b")
	events.publish("a", registryJobJSON{ID: "1"})
	if view := <-mine.ch; view.ID != "1" {
		t.Errorf("the account heard of %q", view.ID)
	}
	select {
	case view := <-other.ch:
		t.Errorf("another account heard of %q", view.ID)
	default:
	}
}

func TestATransferStreamThatFallsBehindIsDropped(t *testing.T) {
	var events transferEvents
	sub := events.subscribe("a")
	for i := 0; i <= eventBacklog; i++ {
		events.publish("a", registryJobJSON{})
	}
	select {
	case <-sub.lost:
	default:
		t.Fatal("the stream that fell behind is kept")
	}
	if len(events.subs) != 0 {
		t.Errorf("%d streams are left", len(events.subs))
	}
	// what was dropped is not told any more
	events.publish("a", registryJobJSON{})
}
