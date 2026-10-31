package config

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/mailsvb/go-fs/internal/features"
)

// Sync keeps a folder of general.baseFolderFiles and a folder of a stored
// server alike, on a schedule: one way, from the local folder to the remote
// one or back, or both ways, where the newer of two files wins. The admin
// interface adds and edits syncs, on the SYNC tab.
type Sync struct {
	// Name names the sync in the admin interface and in the log, and the
	// file go-fs remembers its last run in. Letters, digits, dots, dashes and
	// underscores; unique, whatever its case.
	Name string `toml:"name"`
	// Enabled runs the sync on its schedule. One that is not stays in the
	// file as it is, and does not run.
	Enabled bool `toml:"enabled"`
	// Protocol is how the folders are compared and copied: rsync, which
	// compares the size and the modification time of each file and copies
	// those that differ whole, over SFTP.
	Protocol string `toml:"protocol"`
	// Server is the stored server the remote folder is on, by its name, one
	// of a type the protocol reaches.
	Server string `toml:"server"`
	// RemoteFolder is the folder on the server. A path without a slash in
	// front is from where the login starts; empty is that folder itself.
	RemoteFolder string `toml:"remoteFolder"`
	// LocalFolder is the folder in general.baseFolderFiles, as the file
	// listing shows it, such as /photos. It has to exist.
	LocalFolder string `toml:"localFolder"`
	// Direction is which way files go: push, from the local folder to the
	// remote one; pull, from the remote folder to the local one; or both,
	// where a file changed on one side is copied to the other, and of a file
	// changed on both the newer wins.
	Direction string `toml:"direction"`
	// Delete removes what was removed on the other side. One way, it removes
	// what the source does not have from the target. Both ways, it removes a
	// file from one side once it was removed from the other since the last
	// run. A file removed from the local folder goes to the trash when there
	// is one.
	Delete bool `toml:"delete"`
	// Every runs the sync at this interval, as "15m" or "2h", from when go-fs
	// starts. Either this or At is set.
	Every string `toml:"every,omitempty"`
	// At runs the sync at this time of day, as "02:30", in the time zone of
	// the host, on each of Days.
	At string `toml:"at,omitempty"`
	// Days are the days of the week At is for: mon, tue, wed, thu, fri, sat
	// and sun.
	Days []string `toml:"days,omitempty"`
}

// The directions a sync may go in.
const (
	SyncPush = "push"
	SyncPull = "pull"
	SyncBoth = "both"
)

// SyncDirections are the directions a sync may go in.
var SyncDirections = []string{SyncPush, SyncPull, SyncBoth}

// minSyncInterval is the shortest Every accepted: each run reads both folders
// whole.
const minSyncInterval = time.Minute

// syncDays are the days At may name, by their place in time.Weekday.
var syncDays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Interval is how often the sync runs, 0 for one that runs at a time of day.
func (s Sync) Interval() time.Duration {
	if s.Every == "" {
		return 0
	}
	every, err := time.ParseDuration(s.Every)
	if err != nil {
		return 0
	}
	return every
}

// TimeOfDay is the hour and the minute of At.
func (s Sync) TimeOfDay() (hour, minute int, ok bool) {
	at, err := time.Parse("15:04", s.At)
	if err != nil || s.At != at.Format("15:04") {
		return 0, 0, false
	}
	return at.Hour(), at.Minute(), true
}

// Weekdays are the days At is for.
func (s Sync) Weekdays() []time.Weekday {
	var days []time.Weekday
	for i, day := range syncDays {
		if slices.Contains(s.Days, day) {
			days = append(days, time.Weekday(i))
		}
	}
	return days
}

// SyncProtocol is a way of keeping two folders alike.
type SyncProtocol struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// ServerTypes are the types of server the protocol reaches.
	ServerTypes []string `json:"serverTypes"`

	// feature is the part of go-fs the protocol is.
	feature string
}

// SyncProtocolRsync compares the size and the modification time of each file
// and copies those that differ whole, as rsync does without its delta.
const SyncProtocolRsync = "rsync"

// allSyncProtocols are the protocols a sync may name, whether the binary was
// built with them or not. Another one is added here and given an
// implementation in internal/syncer.
var allSyncProtocols = []SyncProtocol{
	{ID: SyncProtocolRsync, Label: "rsync (SFTP)", ServerTypes: []string{ServerTypeSFTP}, feature: features.SFTPRemote},
}

// SyncProtocols are the protocols a sync may name in this binary: those of
// allSyncProtocols it was built with.
var SyncProtocols = builtSyncProtocols()

func builtSyncProtocols() []SyncProtocol {
	var built []SyncProtocol
	for _, p := range allSyncProtocols {
		if features.Has(p.feature) {
			built = append(built, p)
		}
	}
	return built
}

// SyncProtocolOf looks a protocol up by its ID, among those the binary was
// built with.
func SyncProtocolOf(id string) (SyncProtocol, bool) {
	for _, p := range SyncProtocols {
		if p.ID == id {
			return p, true
		}
	}
	return SyncProtocol{}, false
}

// absentSyncProtocol reports a protocol go-fs knows but this binary was built
// without. A sync of one is kept in the file and otherwise ignored.
func absentSyncProtocol(id string) bool {
	for _, p := range allSyncProtocols {
		if p.ID == id {
			return !features.Has(p.feature)
		}
	}
	return false
}

// UsableSyncs are the syncs of the file the binary runs: those that are
// enabled, of a protocol it was built with, on a server it can reach.
func (c Config) UsableSyncs() []Sync {
	var usable []Sync
	servers := c.UsableServers()
	for _, s := range c.Syncs {
		if !s.Enabled {
			continue
		}
		if _, ok := SyncProtocolOf(s.Protocol); !ok {
			continue
		}
		if _, ok := FindServer(servers, s.Server); !ok {
			continue
		}
		usable = append(usable, s)
	}
	return usable
}

// FindSync finds a sync by its name, whatever its case.
func FindSync(syncs []Sync, name string) (Sync, bool) {
	for _, s := range syncs {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return Sync{}, false
}

// SyncFolder reads the local folder of a sync as it is written in the file: a
// path from the top of the served folder, with a slash in front and none
// behind. Unlike a repository's it may be the top itself.
func SyncFolder(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("name the folder")
	}
	if strings.ContainsAny(raw, "\x00\r\n\\") {
		return "", fmt.Errorf("%q is not a folder", raw)
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("%q leaves the served folder", raw)
		}
	}
	return path.Clean("/" + raw), nil
}

// SyncRemoteFolder reads the remote folder of a sync: clean, "" for where
// the login starts.
func SyncRemoteFolder(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.ContainsAny(raw, "\x00\r\n") {
		return "", fmt.Errorf("%q is not a folder", raw)
	}
	if raw == "" {
		return "", nil
	}
	clean := path.Clean(raw)
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

// RenameSyncServer points the syncs of a server at the name it was given.
func (c *Config) RenameSyncServer(was, name string) {
	for i, s := range c.Syncs {
		if strings.EqualFold(s.Server, was) {
			c.Syncs[i].Server = name
		}
	}
}

// validateSyncs checks the syncs. Their local folders are in the folder the
// servers serve, and their remote ones on stored servers.
func (c Config) validateSyncs() error {
	names := map[string]bool{}
	pairs := map[string]int{}
	for i, s := range c.Syncs {
		where := fmt.Sprintf("syncs[%d]", i)
		if s.Name == "" {
			return fmt.Errorf("%s has no name", where)
		}
		if !repositoryName.MatchString(s.Name) {
			return fmt.Errorf("%s %q: a name is letters, digits, dots, dashes and underscores", where, s.Name)
		}
		if names[strings.ToLower(s.Name)] {
			return fmt.Errorf("%s: %q is configured twice", where, s.Name)
		}
		names[strings.ToLower(s.Name)] = true
		where = fmt.Sprintf("%s %q", where, s.Name)
		if absentSyncProtocol(s.Protocol) {
			// what this binary cannot run it does not check either
			continue
		}
		protocol, ok := SyncProtocolOf(s.Protocol)
		if !ok {
			known := make([]string, len(SyncProtocols))
			for k, p := range SyncProtocols {
				known[k] = p.ID
			}
			return fmt.Errorf("%s: protocol %q is not one of %s", where, s.Protocol, strings.Join(known, ", "))
		}
		server, ok := FindServer(c.Servers, s.Server)
		if !ok {
			return fmt.Errorf("%s: server %q is not a stored server", where, s.Server)
		}
		if absentServer(server) {
			continue
		}
		if !slices.Contains(protocol.ServerTypes, server.Type) {
			return fmt.Errorf("%s: server %q is %s, which %s does not reach", where, s.Server, server.Type, s.Protocol)
		}

		folder, err := SyncFolder(s.LocalFolder)
		if err != nil {
			return fmt.Errorf("%s: localFolder: %w", where, err)
		}
		if folder != s.LocalFolder {
			return fmt.Errorf("%s: localFolder %q has to be %q", where, s.LocalFolder, folder)
		}
		if err := checkFolder(where+": localFolder", RepositoryPath(c.General.BaseFolderFiles, folder)); err != nil {
			return err
		}
		if remote, err := SyncRemoteFolder(s.RemoteFolder); err != nil {
			return fmt.Errorf("%s: remoteFolder: %w", where, err)
		} else if remote != s.RemoteFolder {
			return fmt.Errorf("%s: remoteFolder %q has to be %q", where, s.RemoteFolder, remote)
		}
		if !slices.Contains(SyncDirections, s.Direction) {
			return fmt.Errorf("%s: direction %q is not one of %s", where, s.Direction, strings.Join(SyncDirections, ", "))
		}
		if err := s.validateSchedule(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}

		// two syncs of the same pair of folders would undo each other
		if s.Enabled {
			pair := strings.ToLower(server.Name) + "\x00" + s.RemoteFolder + "\x00" + folder
			if k, seen := pairs[pair]; seen {
				return fmt.Errorf("%s keeps the same folders as syncs[%d]", where, k)
			}
			pairs[pair] = i
		}
	}
	return nil
}

// validateSchedule checks that the sync runs either at an interval or at a
// time of day, and that what it names is one.
func (s Sync) validateSchedule() error {
	switch {
	case s.Every != "" && s.At != "":
		return errors.New("set either every or at, not both")
	case s.Every != "":
		if len(s.Days) > 0 {
			return errors.New("days are for at, not for every")
		}
		every, err := time.ParseDuration(s.Every)
		if err != nil {
			return fmt.Errorf("every %q is not a duration such as 15m or 2h", s.Every)
		}
		if every < minSyncInterval {
			return fmt.Errorf("every has to be at least %s", minSyncInterval)
		}
		if every%time.Minute != 0 {
			return errors.New("every has to be whole minutes")
		}
	case s.At != "":
		if _, _, ok := s.TimeOfDay(); !ok {
			return fmt.Errorf("at %q is not a time of day such as 02:30", s.At)
		}
		if len(s.Days) == 0 {
			return errors.New("name the days at is for")
		}
		seen := map[string]bool{}
		for _, day := range s.Days {
			if !slices.Contains(syncDays, day) {
				return fmt.Errorf("day %q is not one of mon, tue, wed, thu, fri, sat, sun", day)
			}
			if seen[day] {
				return fmt.Errorf("day %q is named twice", day)
			}
			seen[day] = true
		}
	default:
		return errors.New("set every or at, when the sync runs")
	}
	return nil
}
