package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/httpapi"
	"github.com/muhac/actions-runner-pool/internal/reconciler"
	"github.com/muhac/actions-runner-pool/internal/runner"
	"github.com/muhac/actions-runner-pool/internal/scheduler"
	"github.com/muhac/actions-runner-pool/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	st, err := store.OpenSQLite(cfg.StoreDSN)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("failed to close store", "err", err)
		}
	}()

	if existing, err := st.GetAppConfig(context.Background()); err != nil {
		log.Warn("could not load app_config for BASE_URL drift check", "err", err)
	} else if warn, msg := checkBaseURLDrift(existing, cfg.BaseURL); warn {
		log.Warn("BASE_URL drift detected", "details", msg)
	}

	gh := github.NewClient(cfg)
	rn := runner.NewLauncher(cfg)
	sch := scheduler.New(cfg, st, gh, rn, log)

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()

	var schedulerWG sync.WaitGroup
	schedulerWG.Add(1)
	go func() {
		defer schedulerWG.Done()
		if err := sch.RunWithDrain(signalCtx, drainCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("scheduler stopped", "err", err)
		}
	}()

	go func() {
		<-signalCtx.Done()
		log.Info("shutdown signal received; draining scheduler", "timeout", cfg.ShutdownDrainTimeout)
		timer := time.NewTimer(cfg.ShutdownDrainTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			// Hard deadline: drain took too long; cancel it.
			drainCancel()
		case <-drainCtx.Done():
			// drainCtx was cancelled by defer drainCancel() after run() exits
			// (process already done — just let the goroutine clean up the timer).
		}
	}()

	rec := reconciler.New(st, reconciler.NewExecDocker(), gh, log, cfg.RunnerMaxLifetime, cfg.RunnerNamePrefix)
	go func() {
		if err := rec.Run(signalCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("reconciler stopped", "err", err)
		}
	}()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.NewRouter(cfg, st, gh, sch, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-signalCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("gharp listening", "addr", srv.Addr, "base_url", cfg.BaseURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if signalCtx.Err() != nil {
		schedulerWG.Wait()
		log.Info("scheduler drain completed")
	}
	return nil
}
