package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// A folder of [general] that is not there is made, with the folders above it,
// as mkdir -p does, and given the mode its use calls for whatever the umask.
func TestMakeFoldersMakesWhatIsMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "srv", "go-fs")
	cfg := Default()
	cfg.General.BaseFolderFiles = filepath.Join(root, "files")
	cfg.General.BaseFolderRegistry = filepath.Join(root, "registry")
	cfg.General.BaseFolderTemp = filepath.Join(root, "temp")

	if err := cfg.MakeFolders(); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		root:                           0o755,
		cfg.General.BaseFolderFiles:    0o750,
		cfg.General.BaseFolderRegistry: 0o750,
		cfg.General.BaseFolderTemp:     0o700,
	} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Errorf("%s was not made: %v", path, err)
			continue
		}
		// Windows has no such bits to set
		if runtime.GOOS != "windows" && info.Mode().Perm()&^mode != 0 {
			t.Errorf("%s has mode %v, want at most %v", path, info.Mode().Perm(), mode)
		}
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{
			cfg.General.BaseFolderFiles: 0o750,
			cfg.General.BaseFolderTemp:  0o700,
		} {
			if info, _ := os.Stat(path); info.Mode().Perm() != mode {
				t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), mode)
			}
		}
	}
}

// A folder that is there is the operator's, and is left as it is.
func TestMakeFoldersLeavesAnExistingFolderAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits to compare")
	}
	folder := t.TempDir()
	if err := os.Chmod(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.General.BaseFolderTemp = folder
	if err := cfg.MakeFolders(); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(folder); info.Mode().Perm() != 0o755 {
		t.Errorf("the mode of an existing folder was changed to %v", info.Mode().Perm())
	}
}

// An empty folder is not made, and a relative one is left for Validate to
// report rather than made wherever go-fs was started.
func TestMakeFoldersSkipsEmptyAndRelative(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	cfg := Default()
	cfg.General.BaseFolderFiles = "relative"
	if err := cfg.MakeFolders(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "relative")); !os.IsNotExist(err) {
		t.Errorf("a relative folder was made: %v", err)
	}
}

// A folder that cannot be made is reported under its key.
func TestMakeFoldersReportsWhatItCannotMake(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this process cannot write to")
	}
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	cfg := Default()
	cfg.General.BaseFolderRegistry = filepath.Join(locked, "go-fs", "registry")
	err := cfg.MakeFolders()
	if err == nil || !strings.Contains(err.Error(), "general.baseFolderRegistry") {
		t.Errorf("err = %v, want one naming general.baseFolderRegistry", err)
	}
}

// A file whose folders are not there yet loads, which is what lets the
// template serve right after it was written.
func TestLoadMakesTheFolders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "go-fs")
	files := filepath.Join(root, "files")
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	body := "[general]\nbaseFolderFiles = " + strconv.Quote(files) +
		"\nbaseFolderTemp = " + strconv.Quote(filepath.Join(root, "temp")) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	for _, made := range []string{files, filepath.Join(root, "temp")} {
		if info, err := os.Stat(made); err != nil || !info.IsDir() {
			t.Errorf("%s was not made: %v", made, err)
		}
	}
}
