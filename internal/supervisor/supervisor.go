// Package supervisor owns the running servers and keeps them in step with the
// configuration file.
//
// Applying a change is per service: one whose section did not change is left
// alone, one whose accounts or limits changed is reloaded without dropping a
// connection, and one whose port, folder or certificate changed is torn down
// and built again, because those cannot move under a bound listener.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"time"

	"go-fs/internal/config"
	"go-fs/internal/ftp"
	"go-fs/internal/httpd"
	"go-fs/internal/service"
	"go-fs/internal/sftp"
	"go-fs/internal/tftp"
)

// entry describes how one service is switched on, built and updated.
type entry struct {
	name    string
	enabled func(config.Config) bool
	create  func(config.Config, *slog.Logger) (service.Server, error)
	reload  func(service.Server, config.Config) error
}

var services = []entry{
	{
		name:    "ftp",
		enabled: func(c config.Config) bool { return c.FTP.Enabled || c.FTPS.Enabled },
		create: func(c config.Config, log *slog.Logger) (service.Server, error) {
			return ftp.New(c.FTP, c.FTPS, log)
		},
		reload: func(s service.Server, c config.Config) error {
			return s.(*ftp.Server).Reload(c.FTP, c.FTPS)
		},
	},
	{
		name:    "sftp",
		enabled: func(c config.Config) bool { return c.SFTP.Enabled },
		create: func(c config.Config, log *slog.Logger) (service.Server, error) {
			return sftp.New(c.SFTP, log)
		},
		reload: func(s service.Server, c config.Config) error {
			return s.(*sftp.Server).Reload(c.SFTP)
		},
	},
	{
		name:    "http",
		enabled: func(c config.Config) bool { return c.HTTP.Enabled || c.HTTPS.Enabled },
		create: func(c config.Config, log *slog.Logger) (service.Server, error) {
			return httpd.New(c.HTTP, c.HTTPS, log)
		},
		reload: func(s service.Server, c config.Config) error {
			return s.(*httpd.Server).Reload(c.HTTP, c.HTTPS)
		},
	},
	{
		name:    "tftp",
		enabled: func(c config.Config) bool { return c.TFTP.Enabled },
		create: func(c config.Config, log *slog.Logger) (service.Server, error) {
			return tftp.New(c.TFTP, log)
		},
		reload: func(s service.Server, c config.Config) error {
			return s.(*tftp.Server).Reload(c.TFTP)
		},
	},
}

// Supervisor holds what is running.
type Supervisor struct {
	log *slog.Logger

	mu      sync.Mutex
	running map[string]service.Server
	current config.Config
}

func New(logger *slog.Logger) *Supervisor {
	return &Supervisor{log: logger, running: make(map[string]service.Server)}
}

// Apply brings what is running in line with cfg. A service that cannot be
// built or reloaded is reported and left as it was, so a bad section never
// takes down a good one.
func (s *Supervisor) Apply(ctx context.Context, cfg config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range services {
		server, running := s.running[entry.name]
		wanted := entry.enabled(cfg)

		switch {
		case !running && !wanted:
			continue

		case !running && wanted:
			if err := s.start(ctx, entry, cfg); err != nil {
				s.log.Error("cannot start the server", "server", entry.name, "error", err)
			}

		case running && !wanted:
			_ = server.Shutdown(context.Background())
			delete(s.running, entry.name)
			s.log.Info("server stopped", "server", entry.name)

		default:
			err := entry.reload(server, cfg)
			switch {
			case err == nil:
				s.log.Info("server reloaded", "server", entry.name)
			case errors.Is(err, service.ErrNeedsRestart):
				_ = server.Shutdown(context.Background())
				delete(s.running, entry.name)
				if err := s.start(ctx, entry, cfg); err != nil {
					s.log.Error("cannot restart the server", "server", entry.name, "error", err)
					continue
				}
				s.log.Info("server restarted", "server", entry.name,
					"reason", "a setting changed that needs the listener rebound")
			default:
				s.log.Error("cannot reload the server, keeping the running configuration",
					"server", entry.name, "error", err)
			}
		}
	}

	s.current = cfg
	if len(s.running) == 0 {
		return errors.New("no server is enabled, nothing to do")
	}
	return nil
}

// start builds and starts one service.
func (s *Supervisor) start(ctx context.Context, e entry, cfg config.Config) error {
	server, err := e.create(cfg, s.log)
	if err != nil {
		return err
	}
	if err := server.Start(ctx); err != nil {
		return err
	}
	s.running[e.name] = server
	s.log.Info("server started", "server", e.name)
	return nil
}

// Shutdown stops everything.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, server := range s.running {
		_ = server.Shutdown(ctx)
		delete(s.running, name)
	}
}

// Running reports the names of the servers that are up, for tests.
func (s *Supervisor) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.running))
	for _, entry := range services {
		if _, up := s.running[entry.name]; up {
			names = append(names, entry.name)
		}
	}
	return names
}

// stamp is what the watcher compares to notice a change.
type stamp struct {
	size    int64
	modTime time.Time
}

// Watch re-reads path whenever it changes, and on a hangup signal where the
// operating system has one.
//
// The file is polled rather than watched by the kernel: it needs no dependency,
// behaves the same on every platform go-fs is built for, and an editor that
// writes a file in several steps is simply rejected once and picked up on the
// next tick.
func (s *Supervisor) Watch(ctx context.Context, path string, interval time.Duration) {
	last := stampOf(path)

	hangup := make(chan os.Signal, 1)
	if signals := hangupSignals(); len(signals) > 0 {
		signal.Notify(hangup, signals...)
		defer signal.Stop(hangup)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-hangup:
			s.log.Info("reloading the configuration", "reason", "hangup signal", "path", path)
			last = stampOf(path)
			s.reload(ctx, path)

		case <-ticker.C:
			current := stampOf(path)
			if current == last {
				continue
			}
			// the stamp is kept whatever the outcome, so a file that does not
			// parse is reported once rather than on every tick
			last = current
			s.log.Debug("the configuration file changed", "path", path)
			s.reload(ctx, path)
		}
	}
}

// reload loads the file and applies it, unless it says the same thing as what
// is already running.
func (s *Supervisor) reload(ctx context.Context, path string) {
	cfg, err := config.Load(path)
	if err != nil {
		s.log.Error("the configuration was not applied, keeping the running one", "error", err)
		return
	}

	s.mu.Lock()
	unchanged := reflect.DeepEqual(cfg, s.current)
	s.mu.Unlock()
	if unchanged {
		s.log.Debug("the configuration file changed but says the same thing, nothing to do")
		return
	}

	if err := s.Apply(ctx, cfg); err != nil {
		s.log.Error("applying the configuration", "error", err)
	}
}

func stampOf(path string) stamp {
	info, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	return stamp{size: info.Size(), modTime: info.ModTime()}
}
