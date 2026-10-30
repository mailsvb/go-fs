package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MakeFolders makes the folders of [general] that are set but not there yet,
// with every folder above them that is missing, as mkdir -p does. They belong
// to the user go-fs runs as, which is the one that reads and writes them. The
// files and the registry are given 0750 and the temp folder 0700, since
// nobody else has any business in it; a folder above them is given 0755. The
// mode is set after the folder is made, so the umask does not narrow or widen
// it. A folder that is there already is left as it is.
//
// A relative path is left to Validate to report, rather than being made
// relative to wherever go-fs happens to have been started.
func (c Config) MakeFolders() error {
	for _, folder := range []struct {
		name, path string
		mode       os.FileMode
	}{
		{"general.baseFolderFiles", c.General.BaseFolderFiles, 0o750},
		{"general.baseFolderRegistry", c.General.BaseFolderRegistry, 0o750},
		{"general.baseFolderTemp", c.General.BaseFolderTemp, 0o700},
		{"general.baseFolderTrash", c.General.BaseFolderTrash, 0o700},
	} {
		if err := makeFolder(folder.path, folder.mode); err != nil {
			return fmt.Errorf("%s: %w", folder.name, err)
		}
	}
	return nil
}

// makeFolder makes path with mode where it is not there yet.
func makeFolder(path string, mode os.FileMode) error {
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(path, mode); err != nil {
		// made by someone else in the meantime, and theirs to set up
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return os.Chmod(path, mode)
}
