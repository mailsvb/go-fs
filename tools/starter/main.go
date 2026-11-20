// Command starter writes the starter configuration of an installer, the file
// the rpm and the msi ship and copy into place at their first install. It is
// what go-fs itself writes when it starts without a configuration file, so
// the shipped file and the written one never differ.
//
//	go run ./tools/starter -installer msi -o build/pkg/go-fs.msi.toml
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/mailsvb/go-fs/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "starter:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		installer = flag.String("installer", "", "rpm, msi or docker, or empty for a plain binary")
		out       = flag.String("o", "", "file to write")
	)
	flag.Parse()
	if *out == "" {
		return errors.New("-o is required")
	}
	switch *installer {
	case "", "rpm", "msi", "docker":
	default:
		return fmt.Errorf("unknown installer %q: rpm, msi or docker", *installer)
	}
	data, err := config.Starter(*installer)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, data, 0o644)
}
