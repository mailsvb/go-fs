package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/features"
)

func TestSyncs(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	folder := t.TempDir()
	mkdir(t, folder, "photos")
	backup := Server{Name: "backup", Type: ServerTypeSFTP, Host: "sftp.example.com", Username: "u",
		Password: "p", HostKeyFingerprint: "SHA256:x"}
	good := Sync{Name: "photos", Enabled: true, Protocol: SyncProtocolRsync, Server: "backup",
		RemoteFolder: "photos", LocalFolder: "/photos", Direction: SyncPush, Every: "15m"}

	valid := servingConfig(folder)
	valid.Servers = []Server{backup, {Name: "art", Type: ServerTypeArtifactory, URL: "https://x/artifactory", Token: "t"}}
	valid.Syncs = []Sync{good, {Name: "top", Protocol: SyncProtocolRsync, Server: "BACKUP", LocalFolder: "/",
		Direction: SyncBoth, Delete: true, At: "02:30", Days: []string{"mon", "fri"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid sync is refused: %v", err)
	}

	cases := []struct {
		name   string
		change func(*Sync)
		want   string
	}{
		{"no name", func(s *Sync) { s.Name = "" }, "has no name"},
		{"a space in the name", func(s *Sync) { s.Name = "my photos" }, "letters, digits"},
		{"unknown protocol", func(s *Sync) { s.Protocol = "ftp" }, `protocol "ftp" is not one of rsync`},
		{"unknown server", func(s *Sync) { s.Server = "nowhere" }, `"nowhere" is not a stored server`},
		{"a server rsync does not reach", func(s *Sync) { s.Server = "art" }, "which rsync does not reach"},
		{"no local folder", func(s *Sync) { s.LocalFolder = "" }, "name the folder"},
		{"out of the served folder", func(s *Sync) { s.LocalFolder = "/photos/../.." }, "leaves the served folder"},
		{"not normalized", func(s *Sync) { s.LocalFolder = "photos/" }, `has to be "/photos"`},
		{"missing folder", func(s *Sync) { s.LocalFolder = "/missing" }, "missing"},
		{"remote not normalized", func(s *Sync) { s.RemoteFolder = "photos/" }, `has to be "photos"`},
		{"unknown direction", func(s *Sync) { s.Direction = "up" }, `direction "up"`},
		{"no schedule", func(s *Sync) { s.Every = "" }, "set every or at"},
		{"both schedules", func(s *Sync) { s.At = "02:00"; s.Days = []string{"mon"} }, "either every or at"},
		{"bad interval", func(s *Sync) { s.Every = "often" }, "not a duration"},
		{"short interval", func(s *Sync) { s.Every = "30s" }, "at least 1m0s"},
		{"odd interval", func(s *Sync) { s.Every = "90s" }, "whole minutes"},
		{"days with every", func(s *Sync) { s.Days = []string{"mon"} }, "days are for at"},
		{"bad time", func(s *Sync) { s.Every = ""; s.At = "2:30"; s.Days = []string{"mon"} }, "not a time of day"},
		{"no days", func(s *Sync) { s.Every = ""; s.At = "02:30" }, "name the days"},
		{"bad day", func(s *Sync) { s.Every = ""; s.At = "02:30"; s.Days = []string{"monday"} }, `day "monday"`},
		{"a day twice", func(s *Sync) { s.Every = ""; s.At = "02:30"; s.Days = []string{"mon", "mon"} }, "named twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := servingConfig(folder)
			cfg.Servers = valid.Servers
			s := good
			c.change(&s)
			cfg.Syncs = []Sync{s}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}

	t.Run("a name twice", func(t *testing.T) {
		cfg := valid
		other := good
		other.Name = "PHOTOS"
		other.Enabled = false
		cfg.Syncs = []Sync{good, other}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "configured twice") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("the same folders twice", func(t *testing.T) {
		cfg := valid
		other := good
		other.Name = "again"
		other.Direction = SyncPull
		cfg.Syncs = []Sync{good, other}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "the same folders as syncs[0]") {
			t.Errorf("got %v", err)
		}
		// a disabled one is no clash
		other.Enabled = false
		cfg.Syncs = []Sync{good, other}
		if err := cfg.Validate(); err != nil {
			t.Errorf("a disabled sync of the same folders is refused: %v", err)
		}
	})
	t.Run("removing the server", func(t *testing.T) {
		cfg := valid
		cfg.Servers = cfg.Servers[1:]
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `"backup" is not a stored server`) {
			t.Errorf("got %v", err)
		}
	})
}

func TestUsableSyncs(t *testing.T) {
	skipWithout(t, features.SFTPRemote)
	cfg := Default()
	cfg.Servers = []Server{{Name: "backup", Type: ServerTypeSFTP}}
	cfg.Syncs = []Sync{
		{Name: "on", Enabled: true, Protocol: SyncProtocolRsync, Server: "Backup"},
		{Name: "off", Protocol: SyncProtocolRsync, Server: "backup"},
		{Name: "elsewhere", Enabled: true, Protocol: SyncProtocolRsync, Server: "gone"},
	}
	var names []string
	for _, s := range cfg.UsableSyncs() {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"on"}) {
		t.Errorf("usable are %v, want only the enabled one on a stored server", names)
	}
}

func TestRenameSyncServer(t *testing.T) {
	cfg := Default()
	cfg.Syncs = []Sync{{Name: "a", Server: "Backup"}, {Name: "b", Server: "other"}}
	cfg.RenameSyncServer("backup", "vault")
	if cfg.Syncs[0].Server != "vault" || cfg.Syncs[1].Server != "other" {
		t.Errorf("servers are %q and %q", cfg.Syncs[0].Server, cfg.Syncs[1].Server)
	}
}

func TestSyncSchedule(t *testing.T) {
	s := Sync{Every: "90m"}
	if s.Interval() != 90*time.Minute {
		t.Errorf("interval is %s", s.Interval())
	}
	s = Sync{At: "07:05", Days: []string{"sun", "wed"}}
	if h, m, ok := s.TimeOfDay(); !ok || h != 7 || m != 5 {
		t.Errorf("time of day is %d:%d %v", h, m, ok)
	}
	if got := s.Weekdays(); !slices.Equal(got, []time.Weekday{time.Sunday, time.Wednesday}) {
		t.Errorf("weekdays are %v", got)
	}
	if s.Interval() != 0 {
		t.Error("a sync at a time of day has an interval")
	}
}

// TestSyncsRoundTrip writes a sync and reads it back as it was.
func TestSyncsRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Syncs = []Sync{{Name: "photos", Enabled: true, Protocol: SyncProtocolRsync, Server: "backup",
		RemoteFolder: "/srv/photos", LocalFolder: "/photos", Direction: SyncBoth, Delete: true,
		At: "02:30", Days: []string{"mon", "fri"}}}
	path := t.TempDir() + "/go-fs.toml"
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Syncs) != 1 || back.Syncs[0].Name != "photos" || !slices.Equal(back.Syncs[0].Days, []string{"mon", "fri"}) ||
		back.Syncs[0].Every != "" || !back.Syncs[0].Delete {
		t.Errorf("read back %+v", back.Syncs)
	}
}
