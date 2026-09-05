// Command go-fs serves FTP and TFTP from a single configuration file.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go-fs/internal/config"
	"go-fs/internal/ftp"
	"go-fs/internal/httpd"
	"go-fs/internal/sftp"
	"go-fs/internal/tftp"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var servers []interface {
		Shutdown(context.Context) error
	}

	if cfg.FTP.Enabled || cfg.FTPS.Enabled {
		server, err := ftp.New(cfg.FTP, cfg.FTPS, logger)
		if err != nil {
			return err
		}
		if err := server.Start(ctx); err != nil {
			return fmt.Errorf("starting the ftp server: %w", err)
		}
		servers = append(servers, server)
	}

	if cfg.SFTP.Enabled {
		server, err := sftp.New(cfg.SFTP, logger)
		if err != nil {
			shutdownAll(servers)
			return err
		}
		if err := server.Start(ctx); err != nil {
			shutdownAll(servers)
			return fmt.Errorf("starting the sftp server: %w", err)
		}
		servers = append(servers, server)
	}

	if cfg.HTTP.Enabled || cfg.HTTPS.Enabled {
		server, err := httpd.New(cfg.HTTP, cfg.HTTPS, logger)
		if err != nil {
			shutdownAll(servers)
			return err
		}
		if err := server.Start(ctx); err != nil {
			shutdownAll(servers)
			return fmt.Errorf("starting the http server: %w", err)
		}
		servers = append(servers, server)
	}

	if cfg.TFTP.Enabled {
		server, err := tftp.New(cfg.TFTP, logger)
		if err != nil {
			shutdownAll(servers)
			return err
		}
		if err := server.Start(ctx); err != nil {
			shutdownAll(servers)
			return fmt.Errorf("starting the tftp server: %w", err)
		}
		servers = append(servers, server)
	}

	if len(servers) == 0 {
		return errors.New("no server is enabled, nothing to do")
	}

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownAll(servers)
	return nil
}

func shutdownAll(servers []interface{ Shutdown(context.Context) error }) {
	for _, server := range servers {
		_ = server.Shutdown(context.Background())
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
