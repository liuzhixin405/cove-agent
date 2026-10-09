package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/cost"
	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// dreamWorkerProvider builds the worker's provider and the model it runs on
// from the config (profile included), the way the engine does: the provider
// detected from the main model, the background (fast) model for the run. A
// variable so tests can substitute a fake provider.
var dreamWorkerProvider = func(profile string) (api.Provider, string, error) {
	cfg, err := config.LoadWithProfile(profile)
	if err != nil {
		if cfg == nil {
			return nil, "", err
		}
		log.Warnf("config load: %v", err)
	}
	pc := cfg.EffectiveProvider()
	prov := api.DetectProvider(cfg.Model, providerAPIConfig(pc))
	model := cfg.ModelFast
	if model == "" {
		model = cfg.Model
	}
	return prov, model, nil
}

// runDreamWorker is `cove --dream-worker <sessions-dir>`, started detached by
// the exit path (startSessionEndDream) with its output going to dream.log:
// one consolidation of the sessions in sessionsDir, billed to the cost
// history like any other model use, then exit. The result is in
// dream-last.json, which /dream reads. It returns the exit code.
//
// projectRoot (--dream-project) adds that project's memory directory to the
// consolidation; "" consolidates the global directory only.
func runDreamWorker(sessionsDir, projectRoot, profile string) int {
	log.SetLevel(log.Debug) // dream.log is only read when something went wrong
	started := time.Now()
	fmt.Fprintf(os.Stderr, "%s dream worker PID %d: sessions %s\n", started.Format(time.RFC3339), os.Getpid(), sessionsDir)
	prov, model, err := dreamWorkerProvider(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream worker: %v\n", err)
		return 1
	}
	tracker := cost.NewTracker(0)
	metered := api.NewMeteredProvider(prov, func(model string, resp *api.ChatResponse) {
		tracker.AddWithCacheWrite(model, resp.InputTokens, resp.OutputTokens, resp.PromptCacheHitTokens, resp.PromptCacheMissTokens, resp.PromptCacheWriteTokens)
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runErr := dream.RunWorker(ctx, metered, model, sessionsDir, projectRoot)
	if tot := tracker.Totals(); tot.Input+tot.Output > 0 {
		h := cost.NewCostHistory()
		h.Add("dream-worker", model, tracker)
		if err := h.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "dream worker: cost history: %v\n", err)
		}
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "%s dream worker failed after %v: %v\n", time.Now().Format(time.RFC3339), time.Since(started).Round(time.Second), runErr)
		return 1
	}
	fmt.Fprintf(os.Stderr, "%s dream worker done in %v\n", time.Now().Format(time.RFC3339), time.Since(started).Round(time.Second))
	return 0
}
