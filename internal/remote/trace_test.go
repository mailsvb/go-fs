//go:build !light && !no_sftp_remote

package remote_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/logging"
	"github.com/mailsvb/go-fs/internal/remote"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

// A login and what is done on the host are traced to the logger the
// context carries, with the connection and the handshake under them, and
// the password nowhere.
func TestTheHostIsTraced(t *testing.T) {
	host := remotetest.NewSFTPHost(t, "alice", "tracepassword")
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logging.WithLogger(context.Background(), log)
	login := remote.Login{Type: config.ServerTypeSFTP, Host: host.Host, Port: host.Port,
		Username: "alice", Password: "tracepassword", HostKey: host.Fingerprint}

	fsys, err := remote.Open(ctx, login, sshConfig())
	if err != nil {
		t.Fatal(err)
	}
	home, err := fsys.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDir(home); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}

	trace := out.String()
	for _, want := range []string{`msg="conn open" peer="ssh `, `msg="remote ssh handshake complete"`, `msg="remote login"`,
		`login.password=true`, `msg="remote readdir"`, `layer=host`, `msg="remote logout"`, `msg="conn closed"`} {
		if !strings.Contains(trace, want) {
			t.Errorf("the trace lacks %q:\n%s", want, trace)
		}
	}
	if strings.Contains(trace, "tracepassword") {
		t.Errorf("the trace holds the password:\n%s", trace)
	}
}
