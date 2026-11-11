package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/features"
)

// FS is a host once it was logged in to, whatever the protocol: what the file
// listing sends to and the page of a stored server browses. Paths are of the
// host, with slashes, and absolute once Resolve made them so. What goes wrong
// wraps fs.ErrNotExist for a path that is not there and fs.ErrPermission for
// one the login may not reach, which is what a page tells apart.
type FS interface {
	// Resolve makes a path absolute and clean, "" being where the login
	// starts.
	Resolve(p string) (string, error)
	// Stat is what a path is, a link being what it leads to.
	Stat(p string) (fs.FileInfo, error)
	// Lstat is what a path is, a link being the link.
	Lstat(p string) (fs.FileInfo, error)
	// ReadDir lists a folder, a link being what it leads to, without . and
	// ...
	ReadDir(p string) ([]fs.FileInfo, error)
	// Open reads a file, from wherever it is sought to.
	Open(p string) (io.ReadSeekCloser, error)
	// Put writes a file whole, replacing what is there, so that nothing is
	// found under its name before all of it is there. size is how long the
	// body is, -1 when that is not known. It reports how much was written.
	Put(p string, body io.Reader, size int64) (int64, error)
	// Mkdir creates a folder in one that is there.
	Mkdir(p string) error
	// Rename moves a file or a folder to a name that is free.
	Rename(from, to string) error
	// Remove removes a file.
	Remove(p string) error
	// RemoveDir removes a folder with nothing in it, and fails on one that
	// has something.
	RemoveDir(p string) error
	// Close logs out.
	Close() error
}

// Chtimer is a host that can set when a file was modified, which a sync
// keeps alike on both sides to tell a changed file from one that is not.
type Chtimer interface {
	// Chtimes sets the modification time of a file.
	Chtimes(p string, mtime time.Time) error
}

// Open logs in to a host, with a login Checked read, and unlocks its vault
// when it has one. Whatever it holds open lives as long as ctx, which is what
// stops a transfer that hangs.
func Open(ctx context.Context, l Login, sshCfg config.SSH) (FS, error) {
	started := time.Now()
	fsys, err := openHost(ctx, l, sshCfg)
	traceLogin(ctx, "login", l, started, err)
	if err != nil {
		return nil, err
	}
	fsys = traced(fsys, logOf(ctx), l, "host")
	if l.VaultPath == "" {
		return fsys, nil
	}
	started = time.Now()
	vault, err := openVault(fsys, l.VaultPath, l.VaultPassword)
	traceLogin(ctx, "vault unlock", l, started, err)
	if err != nil {
		return nil, err
	}
	return traced(vault, logOf(ctx), l, "vault"), nil
}

// CreateVault logs in to a host, with a login Checked read, and makes a new
// vault at its VaultPath, a folder that is not there yet or is empty, which
// its VaultPassword unlocks. It answers the recovery key of the vault, which
// opens it whatever its password and is not kept anywhere else.
func CreateVault(ctx context.Context, l Login, sshCfg config.SSH) (string, error) {
	if l.VaultPath == "" {
		return "", errors.New("name the folder of the vault")
	}
	if !features.Has(features.Cryptomator) {
		return "", errNoVaults
	}
	started := time.Now()
	fsys, err := openHost(ctx, l, sshCfg)
	traceLogin(ctx, "login", l, started, err)
	if err != nil {
		return "", err
	}
	fsys = traced(fsys, logOf(ctx), l, "host")
	defer func() { _ = fsys.Close() }()
	started = time.Now()
	key, err := createVault(fsys, l.VaultPath, l.VaultPassword)
	// the recovery key is what opens the vault: it is never in a record
	traceLogin(ctx, "vault create", l, started, err)
	return key, err
}

// openHost logs in to a host, without regard to its vault.
func openHost(ctx context.Context, l Login, sshCfg config.SSH) (FS, error) {
	if p, ok := protocols[l.Type]; ok {
		return p.open(ctx, l, sshCfg)
	}
	return nil, fmt.Errorf("%q is not a protocol go-fs knows", l.Type)
}

// protocol is how a host of one of config.ServerTypes is reached.
type protocol struct {
	id string
	// open logs in to a host.
	open func(ctx context.Context, l Login, sshCfg config.SSH) (FS, error)
	// hostKey asks a host for the type and the fingerprint of its key, nil
	// for a protocol whose hosts show none.
	hostKey func(ctx context.Context, l Login, sshCfg config.SSH) (string, string, error)
}

// protocols are the protocols the binary was built with, by their ID. Those
// that are optional, SFTP and SMB, add themselves from their own file, which
// a build tag leaves out together with the module it imports.
var protocols = map[string]protocol{
	config.ServerTypeArtifactory: {id: config.ServerTypeArtifactory,
		open: func(ctx context.Context, l Login, _ config.SSH) (FS, error) {
			return openArtifactory(ctx, l), nil
		}},
}

// registerProtocol adds an optional protocol.
func registerProtocol(p protocol) { protocols[p.id] = p }

// linkInfo is what a link leads to, under the name of the link.
type linkInfo struct {
	fs.FileInfo
	name string
}

func (l linkInfo) Name() string { return l.name }

// IsLink reports an entry of ReadDir that is a link, listed as what it leads
// to: a walk that goes into it might never come out again.
func IsLink(info fs.FileInfo) bool {
	_, ok := info.(linkInfo)
	return ok
}
