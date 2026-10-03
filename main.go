package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/sters/ai-working-calendar/internal/server"
	"github.com/sters/ai-working-calendar/internal/sessionlog"
)

//nolint:gochecknoglobals
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func getVersion() string {
	if version != "dev" {
		return version
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}

	return version
}

func getCommit() string {
	if commit != "none" {
		return commit
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				if len(setting.Value) > 7 {
					return setting.Value[:7]
				}

				return setting.Value
			}
		}
	}

	return commit
}

func getDate() string {
	if date != "unknown" {
		return date
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.time" {
				if t, err := time.Parse(time.RFC3339, setting.Value); err == nil {
					return t.UTC().Format("2006-01-02T15:04:05Z")
				}

				return setting.Value
			}
		}
	}

	return date
}

func main() {
	home, _ := os.UserHomeDir()
	cacheDir, _ := os.UserCacheDir()

	var (
		showVersion bool
		addr        string
		dir         string
		gap         time.Duration
		cachePath   string
	)

	flag.BoolVar(&showVersion, "version", false, "show version information")
	flag.BoolVar(&showVersion, "v", false, "show version information (short)")
	flag.StringVar(&addr, "addr", "127.0.0.1:8137", "listen address")
	flag.StringVar(&dir, "dir", filepath.Join(home, ".claude", "projects"), "Claude Code session log directory")
	flag.DurationVar(&gap, "gap", 30*time.Minute, "idle time that splits a session into separate calendar events")
	flag.StringVar(&cachePath, "cache", filepath.Join(cacheDir, "ai-working-calendar", "index.json"), "index cache file (empty to disable)")
	flag.Parse()

	if showVersion {
		fmt.Printf("Version:    %s\n", getVersion())
		fmt.Printf("Commit:     %s\n", getCommit())
		fmt.Printf("Built:      %s\n", getDate())
		fmt.Printf("Go version: %s\n", runtime.Version())
		fmt.Printf("OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	if err := run(addr, dir, gap, cachePath); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(addr, dir string, gap time.Duration, cachePath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	idx := sessionlog.NewIndex(dir, gap, cachePath)

	// The first scan of a large log directory takes a while; start it before the
	// browser asks so the first page load waits less.
	go func() {
		if _, err := idx.Sessions(); err != nil {
			slog.Warn("initial index", "err", err)
		}
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(idx),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)

	go func() {
		slog.Info("listening", "url", "http://"+addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}
