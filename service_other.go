//go:build !windows

package main

import "log/slog"

// start runs go-fs. Outside Windows a service manager such as systemd runs it
// as it runs any other program, and stops it with SIGTERM.
func start() error {
	return run(nil)
}

// traceService does nothing: there is no service manager to talk to here.
func traceService(*slog.Logger) {}
