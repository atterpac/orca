package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/decisions"
	"github.com/atterpac/orca/internal/discussions"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/registry"
	"github.com/atterpac/orca/internal/storage/sqlite"
	"github.com/atterpac/orca/internal/supervisor"
	"github.com/atterpac/orca/pkg/orca"
	"github.com/atterpac/orca/pkg/runtime/bridge"
	"github.com/atterpac/orca/pkg/runtime/claudecode"
	slackrt "github.com/atterpac/orca/pkg/runtime/slack"
)

// defaultStateDBPath returns the platform-appropriate data-dir location
// for the sqlite state database, following XDG on Linux and the
// conventional per-OS application data dirs elsewhere. Empty string is
// returned on error; callers treat that as "persistence disabled".
func defaultStateDBPath() string {
	var dir string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, "Library", "Application Support", "orca")
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			dir = filepath.Join(v, "orca")
		} else if v := os.Getenv("APPDATA"); v != "" {
			dir = filepath.Join(v, "orca")
		} else {
			return ""
		}
	default:
		// Linux and BSD: XDG_DATA_HOME with XDG fallback.
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			dir = filepath.Join(v, "orca")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			dir = filepath.Join(home, ".local", "share", "orca")
		}
	}
	return filepath.Join(dir, "state.db")
}

// maybeRegisterSlack registers the slack runtime when SLACK_BOT_TOKEN is
// set. Config or init failure is fatal — a present token is a signal
// that the operator expects slack and a silent skip would hide broken
// config. Set ORCA_SLACK_OPTIONAL=1 to downgrade failures to a warning
// (useful for dev loops where slack credentials come and go).
func maybeRegisterSlack(sup *supervisor.Supervisor, b bus.Bus, dec *decisions.Registry, logger *slog.Logger) error {
	if os.Getenv("SLACK_BOT_TOKEN") == "" {
		return nil
	}
	optional := os.Getenv("ORCA_SLACK_OPTIONAL") == "1"
	cfg, err := slackrt.LoadConfig()
	if err != nil {
		if optional {
			logger.Warn("slack config load failed; skipping", "err", err)
			return nil
		}
		return fmt.Errorf("slack config (set ORCA_SLACK_OPTIONAL=1 to run headless): %w", err)
	}
	rt, err := slackrt.New(*cfg, slackrt.Deps{Bus: b, Decisions: dec})
	if err != nil {
		if optional {
			logger.Warn("slack runtime init failed; skipping", "err", err)
			return nil
		}
		return fmt.Errorf("slack runtime init (set ORCA_SLACK_OPTIONAL=1 to run headless): %w", err)
	}
	sup.RegisterRuntime(rt)
	logger.Info("slack runtime registered", "socket_mode", cfg.UseSocketMode())
	return nil
}

// writeEventsLog drains the event subscription into w, one JSON line per
// event. Write errors do not stop the loop; the first failure and every
// 100th subsequent consecutive failure are logged so a stuck disk
// doesn't spam stderr. A successful write after any failures emits a
// recovery log.
//
// Uses json.Marshal + Write rather than json.Encoder: Encoder caches its
// first write error and short-circuits subsequent Encode calls, so a
// transient disk hiccup would become permanent.
func writeEventsLog(w io.Writer, ch <-chan orca.Event, logger *slog.Logger) {
	var consecutiveFails int
	for e := range ch {
		b, err := json.Marshal(e)
		if err != nil {
			logger.Warn("events log marshal failed", "err", err, "kind", e.Kind)
			continue
		}
		b = append(b, '\n')
		if _, err := w.Write(b); err != nil {
			consecutiveFails++
			if consecutiveFails == 1 || consecutiveFails%100 == 0 {
				logger.Warn("events log write failed",
					"err", err,
					"consecutive_fails", consecutiveFails,
				)
			}
			continue
		}
		if consecutiveFails > 0 {
			logger.Info("events log recovered", "after_fails", consecutiveFails)
			consecutiveFails = 0
		}
	}
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	addr := fs.String("addr", ":7878", "listen address")
	eventsLog := fs.String("events-log", "", "if set, append every event as JSONL to this path")
	maxAgents := fs.Int("max-agents", 0, "maximum concurrent agents (0 = unlimited)")
	maxDepth := fs.Int("max-spawn-depth", 0, "maximum dynamic-spawn chain depth (0 = unlimited)")
	stateDB := fs.String("state-db", defaultStateDBPath(), "sqlite file for persistent task state; empty disables persistence")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b := bus.NewInProc()
	ev := events.NewBus(512)
	sup := supervisor.New(b, ev)
	sup.Limits = supervisor.SpawnLimits{MaxAgents: *maxAgents, MaxDepth: *maxDepth}
	sup.RegisterRuntime(claudecode.New())

	if *stateDB != "" {
		if err := os.MkdirAll(filepath.Dir(*stateDB), 0o755); err != nil {
			return fmt.Errorf("state-db dir: %w", err)
		}
		store, err := sqlite.Open(*stateDB)
		if err != nil {
			return fmt.Errorf("open state-db: %w", err)
		}
		defer store.Close()
		sup.SetStore(store)
		if err := sup.LoadTasksFromStore(context.Background()); err != nil {
			return fmt.Errorf("load state-db: %w", err)
		}
		logger.Info("state persistence enabled", "path", *stateDB)
	}

	bridgeRT := bridge.New(b)
	sup.RegisterRuntime(bridgeRT)

	decReg := decisions.New(b, ev, func(id string) bool {
		_, ok := sup.Get(id)
		return ok
	})

	if err := maybeRegisterSlack(sup, b, decReg, logger); err != nil {
		return err
	}

	discReg := discussions.New(ev)
	sup.OnDiscussionTouch = func(bridgeID, agentID, corrID string) {
		discReg.Touch(discussions.TouchInfo{
			ID:            corrID,
			BridgeAgentID: bridgeID,
			Participant:   agentID,
		})
	}
	defer discReg.Stop()

	srv := registry.New(sup, b, ev)
	srv.SetBridgeRuntime(bridgeRT)
	srv.SetDecisions(decReg)
	srv.SetDiscussions(discReg)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *eventsLog != "" {
		f, err := os.OpenFile(*eventsLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open events log: %w", err)
		}
		ch, _ := ev.Subscribe(ctx, events.Filter{})
		go func() {
			defer f.Close()
			writeEventsLog(f, ch, logger)
		}()
		logger.Info("events log enabled", "path", *eventsLog)
	}

	go func() {
		logger.Info("orca daemon listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	sup.Shutdown()
	fmt.Fprintln(os.Stderr, "bye")
	return nil
}
