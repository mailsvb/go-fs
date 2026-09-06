// Command go-fs serves FTP, FTPS, SFTP, HTTP, HTTPS and TFTP from a single
// configuration file.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go-fs/internal/config"
	"go-fs/internal/supervisor"
)

// baseVersion is the released version of the tool and the one place it is
// recorded. The Makefile reads the same file.
//
//go:embed VERSION
var baseVersion string

// version is stamped at build time with -ldflags "-X main.version=...":
// make release stamps the plain version, make build stamps a dev version
// carrying a build timestamp. A plain go build stamps nothing and falls back
// to the embedded version below.
var version string

func currentVersion() string {
	if version != "" {
		return version
	}
	return strings.TrimSpace(baseVersion) + "-dev"
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "go-fs:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "go-fs.toml", "path of the configuration file")
		initPath    = flag.String("init", "", "write a documented starter configuration to this path and exit")
		checkOnly   = flag.Bool("check", false, "load the configuration, report problems and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("go-fs", currentVersion())
		return nil
	}

	if *initPath != "" {
		if _, err := os.Stat(*initPath); err == nil {
			return fmt.Errorf("%s already exists", *initPath)
		}
		if err := os.WriteFile(*initPath, config.Template(), 0o600); err != nil {
			return err
		}
		fmt.Println("wrote", *initPath)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Printf("%s is valid\n", *configPath)
		return nil
	}

	logger := newLogger(cfg.Log)
	warnAboutSecrets(logger, *configPath, cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sup := supervisor.New(logger, *configPath)
	defer sup.Shutdown(context.Background())
	if err := sup.Apply(ctx, cfg); err != nil {
		return err
	}

	if cfg.General.ReloadConfig {
		interval := time.Duration(cfg.General.ReloadInterval) * time.Second
		logger.Info("watching the configuration file", "path", *configPath, "interval", interval)
		go sup.Watch(ctx, *configPath, interval)
	}

	<-ctx.Done()
	logger.Info("shutting down")
	sup.Shutdown(context.Background())
	return nil
}

// warnAboutSecrets says what is worth knowing about the file before the
// listeners come up: that an account still has a password out of the
// documentation, and that the file holding every password and private key can
// be read by more than its owner.
func warnAboutSecrets(logger *slog.Logger, path string, cfg config.Config) {
	if accounts := cfg.ExampleAccounts(); len(accounts) > 0 {
		logger.Warn("an account still has the password this project's own documentation "+
			"prints, so it is a password anybody can look up; change it before this "+
			"server is reachable",
			"accounts", strings.Join(accounts, ", "))
	}
	if mode, loose := config.LooseFilePermissions(path); loose {
		logger.Warn("the configuration file can be read by more than its owner, and it "+
			"holds every password and every private key of this server",
			"path", path, "mode", fmt.Sprintf("%04o", mode), "suggested", "0600")
	}
}

func newLogger(cfg config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, options))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, options))
}
