package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mailsvb/go-fs/internal/features"
	"github.com/mailsvb/go-fs/internal/vfs"
)

// Repository is a package repository go-fs keeps in a folder it serves over
// HTTP. A package put into the folder by any of the servers, or copied there on
// disk, is picked up at the next look at the folder: it is signed with the
// repository's key, the oldest versions of it beyond Keep are removed, and the
// metadata clients read the repository by is written anew and signed. The
// admin interface adds and edits repositories, on the REPOSITORY tab.
type Repository struct {
	// Name names the repository, in the admin interface and as the id of
	// the repository a client adds. Letters, digits, dots, dashes and
	// underscores; unique, whatever its case.
	Name string `toml:"name"`
	// Type is the kind of repository: rpm, for zypper and dnf, fdroid, for
	// the F-Droid app on Android, or jellyfin, a music library Jellyfin
	// clients browse and play.
	Type string `toml:"type"`
	// Folder is where the repository is, a folder of general.baseFolderFiles
	// as the file listing shows it, such as /rpm/go-fs. It has to exist.
	Folder string `toml:"folder"`
	// SigningKey is the key the repository signs with, base64 as the other
	// keys in this file are, without a passphrase, and RSA only. For rpm it
	// is an armored OpenPGP private key, which signs the packages and the
	// metadata. For fdroid it is a certificate and its private key, PEM, one
	// after the other, which sign the index; clients add the repository by
	// the fingerprint of the certificate, so it is not to change. It is kept
	// as it is, so keep this file private. A jellyfin repository signs
	// nothing and has none.
	SigningKey string `toml:"signingKey"`
	// Keep is how many versions of each package, by name and architecture,
	// stay in the folder; older ones are removed. 0 keeps every version. An
	// APK is a version of its package name, by its versionCode. A jellyfin
	// repository removes nothing, and keep is 0 for it.
	Keep int `toml:"keep"`
	// PollInterval is how often, in seconds, the folder is looked at for
	// packages that were added, replaced or removed. 60 when not set.
	PollInterval int `toml:"pollInterval,omitempty"`
	// URL is where clients reach the folder, such as
	// https://files.example.com/rpm/go-fs. When set, the repository writes
	// <name>.repo into the folder, the definition zypper and dnf add. An
	// fdroid repository has to have one, ending in /repo, which is the
	// address the client is given and the index names.
	URL string `toml:"url,omitempty"`
	// Title is the name the F-Droid app shows of an fdroid repository, or
	// the name of the library Jellyfin clients show of a jellyfin one; the
	// name when not set.
	Title string `toml:"title,omitempty" feature:"fdroid,jellyfin"`
	// Description is the text the F-Droid app shows of an fdroid
	// repository.
	Description string `toml:"description,omitempty" feature:"fdroid"`
	// Icon is the image the F-Droid app shows of an fdroid repository, a
	// file in its folder by its path in it, such as /icon.png.
	Icon string `toml:"icon,omitempty" feature:"fdroid"`
}

// DisplayName is the name a client shows of the repository.
func (r Repository) DisplayName() string {
	if r.Title != "" {
		return r.Title
	}
	return r.Name
}

// DefaultPollInterval is how often a repository's folder is looked at when
// its pollInterval is not set, in seconds.
const DefaultPollInterval = 60

// minPollInterval is the shortest pollInterval accepted: each look reads the
// whole folder.
const minPollInterval = 5

// Interval is how often the folder is looked at, in seconds.
func (r Repository) Interval() int {
	if r.PollInterval == 0 {
		return DefaultPollInterval
	}
	return r.PollInterval
}

// RepositoryType is a kind of repository.
type RepositoryType struct {
	ID    string `json:"id"`
	Label string `json:"label"`

	// feature is the part of go-fs the kind is.
	feature string
}

// RepositoryTypeRPM is an rpm-md repository, read by zypper and dnf.
const RepositoryTypeRPM = "rpm"

// RepositoryTypeFDroid is an F-Droid repository of APKs, read by the F-Droid
// app on Android.
const RepositoryTypeFDroid = "fdroid"

// RepositoryTypeJellyfin is a music library, which Jellyfin clients browse
// and play over the HTTP server.
const RepositoryTypeJellyfin = "jellyfin"

// allRepositoryTypes are the kinds a repository may be, whether the binary was
// built with them or not. Another one is added here and given an
// implementation in internal/repository.
var allRepositoryTypes = []RepositoryType{
	{ID: RepositoryTypeRPM, Label: "rpm (zypper, dnf)", feature: features.RPM},
	{ID: RepositoryTypeFDroid, Label: "F-Droid (Android)", feature: features.FDroid},
	{ID: RepositoryTypeJellyfin, Label: "Jellyfin (music)", feature: features.Jellyfin},
}

// RepositoryTypes are the kinds a repository may be in this binary: those of
// allRepositoryTypes it was built with.
var RepositoryTypes = builtRepositoryTypes()

func builtRepositoryTypes() []RepositoryType {
	var built []RepositoryType
	for _, t := range allRepositoryTypes {
		if features.Has(t.feature) {
			built = append(built, t)
		}
	}
	return built
}

// absentRepositoryType reports a kind of repository go-fs knows but this
// binary was built without. A repository of one is kept in the file and
// otherwise ignored, so that go-fs-light reads the file of go-fs.
func absentRepositoryType(id string) bool {
	for _, t := range allRepositoryTypes {
		if t.ID == id {
			return !features.Has(t.feature)
		}
	}
	return false
}

// UsableRepositories are the repositories of the file the binary can keep:
// those whose kind it was built with.
func (c Config) UsableRepositories() []Repository {
	var usable []Repository
	for _, repo := range c.Repositories {
		if _, ok := RepositoryTypeOf(repo.Type); ok {
			usable = append(usable, repo)
		}
	}
	return usable
}

// NeedsSigningKey reports whether a repository of type typ signs, and has to
// have a signingKey: a jellyfin repository writes nothing a client checks.
func NeedsSigningKey(typ string) bool {
	return typ != RepositoryTypeJellyfin
}

// SigningKeyKind is the kind of key material a repository of type typ signs
// with, "" for one that signs nothing.
func SigningKeyKind(typ string) string {
	switch typ {
	case RepositoryTypeFDroid:
		return KindJARKey
	case RepositoryTypeJellyfin:
		return ""
	}
	return KindPGPKey
}

// JellyfinFolders are the folders of the jellyfin repositories the binary
// keeps, which only an account reads.
func (c Config) JellyfinFolders() []string {
	var folders []string
	for _, repo := range c.UsableRepositories() {
		if repo.Type == RepositoryTypeJellyfin {
			folders = append(folders, repo.Folder)
		}
	}
	return folders
}

// JellyfinServer is what Jellyfin clients are told of the server: its name,
// the names the jellyfin repositories show, and the address to reach it at,
// the first url one of them sets, "" for none.
func (c Config) JellyfinServer() (name, address string) {
	var names []string
	for _, repo := range c.UsableRepositories() {
		if repo.Type != RepositoryTypeJellyfin {
			continue
		}
		names = append(names, repo.DisplayName())
		if address == "" {
			address = repo.URL
		}
	}
	return strings.Join(names, ", "), address
}

// RepositoryTypeOf looks a kind of repository up by its ID.
func RepositoryTypeOf(id string) (RepositoryType, bool) {
	for _, t := range RepositoryTypes {
		if t.ID == id {
			return t, true
		}
	}
	return RepositoryType{}, false
}

// repositoryName is what a name may be: it is the id a client gives the
// repository, and the name of the .repo file.
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// RepositoryFolder reads the folder of a repository as it is written in the
// file: a path from the top of the served folder, with a slash in front and
// none behind. The top itself is not one: the repository writes into its
// folder.
func RepositoryFolder(raw string) (string, error) {
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
	clean := path.Clean("/" + raw)
	if clean == "/" {
		return "", errors.New("the repository needs a folder of its own, not the top of the served folder")
	}
	if err := notReserved(raw, clean); err != nil {
		return "", err
	}
	return clean, nil
}

// notReserved refuses a folder of the served tree in the folder go-fs keeps
// for itself at its top, which no protocol reaches. The name is matched
// ignoring case, as it is wherever the filesystem does: a folder that only
// differs in case is too close to be worth telling apart in a setting.
func notReserved(raw, clean string) error {
	first, _, _ := strings.Cut(strings.TrimPrefix(clean, "/"), "/")
	if strings.EqualFold(first, vfs.ReservedName) {
		return fmt.Errorf("%q is in /%s, which go-fs keeps for itself", raw, vfs.ReservedName)
	}
	return nil
}

// RepositoryPath is where the folder of the repository is on disk, inside
// basefolder, the folder the servers serve.
func RepositoryPath(basefolder, folder string) string {
	return filepath.Join(basefolder, filepath.FromSlash(strings.TrimPrefix(folder, "/")))
}

// RepositoryURL reads the address clients reach a repository at, without a
// slash at its end; "" is none.
func RepositoryURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("%q is not an address", raw)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q has to begin with https:// or http://", raw)
	case u.Host == "":
		return "", fmt.Errorf("%q names no host", raw)
	case u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("%q has to be the address alone, without a query", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// FindRepository finds a repository by its name, whatever its case.
func FindRepository(repositories []Repository, name string) (Repository, bool) {
	for _, r := range repositories {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return Repository{}, false
}

// validateRepositories checks the package repositories. Their folders are in
// the folder the http server serves, which is where clients read them.
func (c Config) validateRepositories() error {
	names := map[string]bool{}
	var folders []string
	for i, repo := range c.Repositories {
		where := fmt.Sprintf("repositories[%d]", i)
		if repo.Name == "" {
			return fmt.Errorf("%s has no name", where)
		}
		if !repositoryName.MatchString(repo.Name) {
			return fmt.Errorf("%s %q: a name is letters, digits, dots, dashes and underscores", where, repo.Name)
		}
		if names[strings.ToLower(repo.Name)] {
			return fmt.Errorf("%s: %q is configured twice", where, repo.Name)
		}
		names[strings.ToLower(repo.Name)] = true
		where = fmt.Sprintf("%s %q", where, repo.Name)
		if absentRepositoryType(repo.Type) {
			// what this binary cannot keep it does not check either
			continue
		}

		if _, ok := RepositoryTypeOf(repo.Type); !ok {
			known := make([]string, len(RepositoryTypes))
			for k, t := range RepositoryTypes {
				known[k] = t.ID
			}
			return fmt.Errorf("%s: type %q is not one of %s", where, repo.Type, strings.Join(known, ", "))
		}
		folder, err := RepositoryFolder(repo.Folder)
		if err != nil {
			return fmt.Errorf("%s: folder: %w", where, err)
		}
		if folder != repo.Folder {
			return fmt.Errorf("%s: folder %q has to be %q", where, repo.Folder, folder)
		}
		onDisk := RepositoryPath(c.General.BaseFolderFiles, folder)
		if err := checkFolder(where+": folder", onDisk); err != nil {
			return err
		}
		for k, other := range folders {
			if overlaps(comparablePath(onDisk), comparablePath(other)) {
				return fmt.Errorf("%s: folder %q overlaps the folder of repositories[%d]", where, folder, k)
			}
		}
		folders = append(folders, onDisk)

		switch {
		case !NeedsSigningKey(repo.Type):
			if strings.TrimSpace(repo.SigningKey) != "" {
				return fmt.Errorf("%s: a %s repository signs nothing and has no signingKey", where, repo.Type)
			}
		case strings.TrimSpace(repo.SigningKey) == "":
			return fmt.Errorf("%s has no signingKey", where)
		default:
			if _, err := Decode(SigningKeyKind(repo.Type), repo.SigningKey); err != nil {
				return fmt.Errorf("%s: signingKey %w", where, err)
			}
		}
		if repo.Keep < 0 {
			return fmt.Errorf("%s: keep cannot be negative", where)
		}
		if repo.PollInterval != 0 && repo.PollInterval < minPollInterval {
			return fmt.Errorf("%s: pollInterval has to be at least %d seconds", where, minPollInterval)
		}
		if address, err := RepositoryURL(repo.URL); err != nil {
			return fmt.Errorf("%s: url: %w", where, err)
		} else if address != repo.URL {
			return fmt.Errorf("%s: url %q has to be %q", where, repo.URL, address)
		}
		if err := repo.validateFDroid(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if err := repo.validateJellyfin(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

// maxTitle is the longest title the F-Droid app shows whole.
const maxTitle = 50

// validateFDroid checks what is particular to an fdroid repository, and
// that a repository of another type sets none of it.
func (r Repository) validateFDroid() error {
	if r.Type != RepositoryTypeFDroid {
		if r.Title != "" && r.Type != RepositoryTypeJellyfin {
			return errors.New("title is for an fdroid or a jellyfin repository only")
		}
		for key, value := range map[string]string{"description": r.Description, "icon": r.Icon} {
			if value != "" {
				return fmt.Errorf("%s is for an fdroid repository only", key)
			}
		}
		return nil
	}
	if r.URL == "" {
		return errors.New("an fdroid repository needs its url, the address the F-Droid app is given")
	}
	if u, _ := url.Parse(r.URL); !strings.HasSuffix(u.Path, "/repo") {
		// the client appends /fdroid/repo to any other address it is given
		return fmt.Errorf("url %q has to end in /repo, or the F-Droid app looks for the repository at %s/fdroid/repo",
			r.URL, r.URL)
	}
	if len([]rune(r.Title)) > maxTitle {
		return fmt.Errorf("title has to be at most %d characters", maxTitle)
	}
	if r.Icon != "" {
		icon, err := RepositoryFolder(r.Icon)
		if err != nil {
			return fmt.Errorf("icon: %w", err)
		}
		if icon != r.Icon {
			return fmt.Errorf("icon %q has to be %q", r.Icon, icon)
		}
		ext := strings.ToLower(path.Ext(icon))
		if ext != ".png" && ext != ".jpg" && ext != ".webp" {
			return fmt.Errorf("icon %q has to be a .png, .jpg or .webp image", r.Icon)
		}
	}
	return nil
}

// validateJellyfin checks what is particular to a jellyfin repository: its
// files are music, of which there are no versions to remove.
func (r Repository) validateJellyfin() error {
	if r.Type != RepositoryTypeJellyfin {
		return nil
	}
	if r.Keep != 0 {
		return errors.New("keep is 0 for a jellyfin repository, which removes nothing")
	}
	if len([]rune(r.Title)) > maxTitle {
		return fmt.Errorf("title has to be at most %d characters", maxTitle)
	}
	return nil
}

// KindPGPKey is the OpenPGP signing key of an rpm repository.
const KindPGPKey = "pgpkey"

// KindJARKey is the key the index of an fdroid repository is signed with.
const KindJARKey = "jarkey"
