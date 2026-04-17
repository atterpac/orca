package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atterpac/orca/internal/bus"
	"github.com/atterpac/orca/internal/decisions"
	"github.com/atterpac/orca/internal/discussions"
	"github.com/atterpac/orca/internal/events"
	"github.com/atterpac/orca/internal/registry"
	"github.com/atterpac/orca/internal/supervisor"
	"github.com/atterpac/orca/pkg/runtime/bridge"
	"github.com/atterpac/orca/pkg/runtime/claudecode"
	slackrt "github.com/atterpac/orca/pkg/runtime/slack"
)

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	addr := fs.String("addr", ":7878", "listen address")
	eventsLog := fs.String("events-log", "", "if set, append every event as JSONL to this path")
	maxAgents := fs.Int("max-agents", 0, "maximum concurrent agents (0 = unlimited)")
	maxDepth := fs.Int("max-spawn-depth", 0, "maximum dynamic-spawn chain depth (0 = unlimited)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b := bus.NewInProc()
	ev := events.NewBus(512)
	sup := supervisor.New(b, ev)
	sup.Limits = supervisor.SpawnLimits{MaxAgents: *maxAgents, MaxDepth: *maxDepth}
	sup.RegisterRuntime(claudecode.New())

	bridgeRT := bridge.New(b)
	sup.RegisterRuntime(bridgeRT)

	decReg := decisions.New(b, ev, func(id string) bool {
		_, ok := sup.Get(id)
		return ok
	})

	// Slack runtime: opt-in. Registers only when SLACK_BOT_TOKEN is present.
	// Skips silently when unconfigured so the daemon still runs headless.
	if os.Getenv("SLACK_BOT_TOKEN") != "" {
		slackCfg, err := slackrt.LoadConfig()
		if err != nil {
			logger.Warn("slack runtime skipped", "err", err)
		} else {
			slackRT, err := slackrt.New(*slackCfg, slackrt.Deps{
				Bus:       b,
				Decisions: decReg,
			})
			if err != nil {
				logger.Warn("slack runtime init failed", "err", err)
			} else {
				sup.RegisterRuntime(slackRT)
				logger.Info("slack runtime registered",
					"socket_mode", slackCfg.UseSocketMode())
			}
		}
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
			enc := json.NewEncoder(f)
			for e := range ch {
				if err := enc.Encode(e); err != nil {
					return
				}
			}
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
