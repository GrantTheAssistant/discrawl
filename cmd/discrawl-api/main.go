package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openclaw/discrawl/internal/archiveapi"
	"github.com/openclaw/discrawl/internal/projection"
	"github.com/openclaw/discrawl/internal/store"
)

func main() {
	configPath := flag.String("config", "/etc/discrawl/archive-api.json", "archive API JSON config")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := archiveapi.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	api, err := archiveapi.NewServer(cfg, logger)
	if err != nil {
		logger.Error("initialize archive API", "error", err)
		os.Exit(1)
	}
	defer func() { _ = api.Close() }()

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	var projector *projection.Projector
	projectorErrors := make(chan error, 1)
	if cfg.Projection.Enabled {
		archive, openErr := store.OpenReadOnly(runCtx, cfg.DBPath)
		if openErr != nil {
			logger.Error("open projection archive", "error", openErr)
			os.Exit(1)
		}
		defer func() { _ = archive.Close() }()
		firebaseSink, sinkErr := projection.NewFirebaseSink(runCtx, projection.FirebaseConfig{
			ProjectID: cfg.Projection.ProjectID, OrgID: cfg.Projection.OrgID, DatabaseURL: cfg.Projection.DatabaseURL,
		})
		if sinkErr != nil {
			logger.Error("initialize tenant-local projection sink", "error", sinkErr)
			os.Exit(1)
		}
		parse := func(raw string) time.Duration { value, _ := time.ParseDuration(raw); return value }
		projector, err = projection.New(projection.Config{
			GuildID: cfg.GuildID, PollEvery: parse(cfg.Projection.PollEvery),
			BindingsEvery: parse(cfg.Projection.BindingsEvery), RepairEvery: parse(cfg.Projection.RepairEvery),
			RepairLookback: parse(cfg.Projection.RepairLookback), InitialLookback: parse(cfg.Projection.InitialLookback),
			InitialRowsPerBinding: cfg.Projection.InitialRowsPerBinding, BatchSize: cfg.Projection.BatchSize,
			StatePath: cfg.Projection.StatePath, OperationTimeout: parse(cfg.Projection.OperationTimeout),
			StatusEvery: parse(cfg.Projection.StatusEvery),
		}, archive, firebaseSink, logger)
		if err != nil {
			_ = firebaseSink.Close()
			logger.Error("initialize projection", "error", err)
			os.Exit(1)
		}
		defer func() { _ = projector.Close() }()
		go func() { projectorErrors <- projector.Run(runCtx) }()
	}

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		logger.Info("discrawl archive API listening", "address", cfg.Listen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("archive API stopped", "error", err)
			os.Exit(1)
		}
	}()

	shutdown, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	projectionFailed := false
	projectorJoined := false
	if projector == nil {
		<-shutdown.Done()
	} else {
		select {
		case <-shutdown.Done():
		case runErr := <-projectorErrors:
			projectorJoined = true
			projectionFailed = true
			logger.Error("projection stopped unexpectedly", "error", runErr)
		}
	}
	cancelRun()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("archive API shutdown", "error", err)
		os.Exit(1)
	}
	if projector != nil && !projectorJoined {
		select {
		case runErr := <-projectorErrors:
			if runErr != nil {
				logger.Error("projection shutdown", "error", runErr)
				projectionFailed = true
			}
		case <-time.After(20 * time.Second):
			logger.Error("projection shutdown timed out")
			projectionFailed = true
		}
	}
	if projectionFailed {
		os.Exit(1)
	}
}
