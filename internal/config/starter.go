package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// starterEdits are the lines of the template that differ for the build an
// installer made, each replaced whole. Every line named must be in the
// template, so a template that drops one fails the tests rather than giving
// an installer a starter that serves from the wrong folder.
var starterEdits = map[string][][2]string{
	// the msi keeps everything under ProgramData, beside its configuration
	"msi": {
		{`baseFolderFiles = "/var/lib/go-fs/files"`, `baseFolderFiles = "C:/ProgramData/go-fs/files"`},
		{`baseFolderRegistry = "/var/lib/go-fs/registry"`, `baseFolderRegistry = "C:/ProgramData/go-fs/registry"`},
		{`baseFolderTemp = "/var/lib/go-fs/temp"`, `baseFolderTemp = "C:/ProgramData/go-fs/temp"`},
		{`baseFolderTrash = "/var/lib/go-fs/trash"`, `baseFolderTrash = "C:/ProgramData/go-fs/trash"`},
		{`baseFolderDisks = "/var/lib/go-fs/disks"`, `baseFolderDisks = "C:/ProgramData/go-fs/disks"`},
	},
	// in a container every server listens on all its interfaces, since a
	// published port does not reach 127.0.0.1 inside it
	"docker": {
		{`address = "127.0.0.1"`, `address = ""`},
	},
}

// Starter is the documented starter configuration as the build an installer
// made starts with: rpm, msi or docker, as main.packaging says, or "" for a
// binary of no installer. The rpm's and a plain binary's is the template
// itself, which serves from /var/lib/go-fs.
func Starter(installer string) ([]byte, error) {
	data := bytes.Clone(Template())
	for _, edit := range starterEdits[installer] {
		lines := bytes.Split(data, []byte("\n"))
		found := false
		for i, line := range lines {
			if string(bytes.TrimSuffix(line, []byte("\r"))) == edit[0] {
				lines[i] = []byte(edit[1])
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("the starter configuration has no line %s to adjust for %s", edit[0], installer)
		}
		data = bytes.Join(lines, []byte("\n"))
	}
	return data, nil
}

// WriteStarter writes the starter configuration of installer to path, readable
// by its owner only, and makes the folder it is in when there is none. A file
// that is there already is left as it is, even one made a moment before:
// wrote says whether this call wrote it.
func WriteStarter(path, installer string) (wrote bool, err error) {
	data, err := Starter(installer)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return false, err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return false, err
	}
	return true, nil
}
