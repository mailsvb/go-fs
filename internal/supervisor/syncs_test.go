package supervisor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/remote/remotetest"
)

func TestSyncsRunBesideTheServers(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	sup, logs, ctx := newSupervisor(t)
	cfg := baseConfig(t)
	host := remotetest.NewSFTPHost(t, "alice", "secret")
	if err := os.Mkdir(filepath.Join(cfg.General.BaseFolderFiles, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.General.BaseFolderFiles, "photos", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Servers = []config.Server{{Name: "backup", Type: config.ServerTypeSFTP, Host: host.Host, Port: host.Port,
		Username: "alice", Password: "secret", HostKeyFingerprint: host.Fingerprint}}
	cfg.Syncs = []config.Sync{{Name: "photos", Enabled: true, Protocol: config.SyncProtocolRsync, Server: "backup",
		LocalFolder: "/photos", Direction: config.SyncPush, Every: "1h"}}

	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	logs.waitFor(t, "server started server=syncs")
	logs.waitFor(t, "sync done")
	if got := sup.Running(); !slices.Contains(got, "syncs") {
		t.Fatalf("running = %v, want syncs", got)
	}
	if body, err := os.ReadFile(filepath.Join(host.Dir, "a.txt")); err != nil || string(body) != "a" {
		t.Errorf("the file was not synced: %q %v", body, err)
	}

	// a disabled sync stops the service
	cfg.Syncs[0].Enabled = false
	if err := sup.Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := sup.Running(); slices.Contains(got, "syncs") {
		t.Errorf("running = %v with no sync enabled", got)
	}
}
