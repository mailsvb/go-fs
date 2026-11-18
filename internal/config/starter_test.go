package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A plain binary and the rpm start from the template itself, as does a build
// stamped with an installer this code does not know.
func TestTheStarterOfThePlainBinaryAndTheRpmIsTheTemplate(t *testing.T) {
	for _, installer := range []string{"", "rpm", "something-else"} {
		data, err := Starter(installer)
		if err != nil {
			t.Fatalf("%q: %v", installer, err)
		}
		if !bytes.Equal(data, Template()) {
			t.Errorf("the starter of %q is not the template", installer)
		}
	}
}

// The msi's starter serves from ProgramData, and names no Linux folder.
func TestTheStarterOfTheMsiServesFromProgramData(t *testing.T) {
	data, err := Starter("msi")
	if err != nil {
		t.Fatal(err)
	}
	// the examples in the comments may say what they like
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "/var/lib/go-fs") {
			t.Errorf("the msi's starter still names a folder under /var/lib/go-fs: %s", line)
		}
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for key, got := range map[string]string{
		"files":    cfg.General.BaseFolderFiles,
		"registry": cfg.General.BaseFolderRegistry,
		"temp":     cfg.General.BaseFolderTemp,
		"trash":    cfg.General.BaseFolderTrash,
		"disks":    cfg.General.BaseFolderDisks,
	} {
		if want := "C:/ProgramData/go-fs/" + key; got != want {
			t.Errorf("the %s folder of the msi's starter is %q, want %q", key, got, want)
		}
	}
}

// The image's starter listens on every interface of the container, and loads
// as it stands once its folders are somewhere a test may make them.
func TestTheStarterOfTheImageListensOnAllInterfaces(t *testing.T) {
	data, err := Starter("docker")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("\naddress = \"127.0.0.1\"")) {
		t.Error("the image's starter still listens on 127.0.0.1")
	}
	root := t.TempDir()
	body := strings.ReplaceAll(string(data), "/var/lib/go-fs", filepath.ToSlash(root))
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the image's starter has to load: %v", err)
	}
	for name, address := range map[string]string{
		"ftp": cfg.FTP.Address, "sftp": cfg.SFTP.Address, "http": cfg.HTTP.Address,
		"tftp": cfg.TFTP.Address, "nbd": cfg.NBD.Address,
	} {
		if address != "" {
			t.Errorf("%s.address of the image's starter is %q, want all interfaces", name, address)
		}
	}
}

// Every line an installer adjusts has to be in the template: a template that
// lost one would otherwise hand that installer a starter it was not made for.
func TestEveryStarterIsBuiltFromLinesTheTemplateHas(t *testing.T) {
	for installer := range starterEdits {
		if _, err := Starter(installer); err != nil {
			t.Errorf("%s: %v", installer, err)
		}
	}
}

func TestWriteStarterWritesAMissingFileForItsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not", "there", "go-fs.toml")
	wrote, err := WriteStarter(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("WriteStarter says it wrote nothing")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, Template()) {
		t.Error("the file written is not the starter")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("the starter has mode %o, want 600", mode)
		}
	}
}

func TestWriteStarterLeavesAFileThatIsThereAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go-fs.toml")
	mine := []byte("[general]\n# mine\n")
	if err := os.WriteFile(path, mine, 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err := WriteStarter(path, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("WriteStarter says it wrote over a file that was there")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, mine) {
		t.Error("WriteStarter changed a file that was there")
	}
}

func TestWriteStarterFailsInAFolderItCannotWriteTo(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this process cannot write to")
	}
	folder := t.TempDir()
	if err := os.Chmod(folder, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(folder, 0o700) })
	path := filepath.Join(folder, "go-fs.toml")
	wrote, err := WriteStarter(path, "")
	if err == nil {
		t.Fatal("WriteStarter wrote into a folder it may not write to")
	}
	if wrote {
		t.Error("WriteStarter failed, but says it wrote")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a failed WriteStarter left a file behind")
	}
}
