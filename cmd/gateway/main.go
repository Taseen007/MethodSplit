// Command gateway is the MethodSplit reverse proxy: the single public entry
// point that splits read and write traffic, protects against duplicate
// payments, rate-limits, and (optionally) flags anomalous clients.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Taseen007/MethodSplit/internal/config"
	"github.com/Taseen007/MethodSplit/internal/gateway"
	"github.com/Taseen007/MethodSplit/internal/metrics"
	"github.com/Taseen007/MethodSplit/internal/ml"
	"github.com/Taseen007/MethodSplit/internal/redisx"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	gatewayConfigPath := envOr("GATEWAY_CONFIG", "configs/gateway.yaml")
	merchantsConfigPath := envOr("MERCHANTS_CONFIG", "configs/merchants.yaml")
	addr := envOr("GATEWAY_ADDR", ":8080")
	redisAddr := envOr("REDIS_ADDR", "localhost:6379")

	cfg, err := config.LoadGateway(gatewayConfigPath)
	if err != nil {
		logger.Error("load gateway config", "error", err)
		os.Exit(1)
	}
	merchants, err := config.LoadMerchants(merchantsConfigPath)
	if err != nil {
		logger.Error("load merchants config", "error", err)
		os.Exit(1)
	}

	redisClient := redisx.New(redisAddr)
	defer redisClient.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := redisClient.Ping(pingCtx); err != nil {
		logger.Warn("redis not reachable at startup; the gateway will apply its fail-open/fail-closed policy per request", "error", err)
	}
	pingCancel()

	gw, err := gateway.New(cfg, merchants, redisClient, logger)
	if err != nil {
		logger.Error("build gateway", "error", err)
		os.Exit(1)
	}

	{
		// The feature log is written even in ml.mode: off, to collect
		// training data (ARCHITECTURE.md §12.6).
		featuresLogPath := envOr("ML_FEATURES_LOG", "logs/features.jsonl")
		if err := os.MkdirAll(filepath.Dir(featuresLogPath), 0o755); err != nil {
			logger.Warn("create features log dir", "error", err)
		}
		featuresLogger, err := ml.NewFeatureLogger(featuresLogPath)
		if err != nil {
			logger.Warn("open features log; ML feature logging disabled", "error", err)
			featuresLogger = nil
		} else {
			defer featuresLogger.Close()
		}

		guard := ml.NewGuard(cfg.ML.Mode, redisClient, featuresLogger, logger)
		if cfg.ML.Mode != config.MLModeOff {
			loadModel(guard, cfg.ML.ModelPath, logger)
		}
		gw.SetMLGuard(guard)

		if cfg.ML.Mode != config.MLModeOff {
			sighup := make(chan os.Signal, 1)
			signal.Notify(sighup, syscall.SIGHUP)
			go func() {
				for range sighup {
					logger.Info("SIGHUP received; reloading ML model", "path", cfg.ML.ModelPath)
					loadModel(guard, cfg.ML.ModelPath, logger)
				}
			}()
		}
	}

	healthChecker := gateway.NewHealthChecker(cfg.HealthCheck)
	go healthChecker.Run(ctx, gw.Pools())

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", gw)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("gateway starting", "addr", addr, "split_enabled", cfg.SplitEnabled, "ml_mode", cfg.ML.Mode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadModel(guard *ml.Guard, path string, logger *slog.Logger) {
	model, err := ml.LoadModel(path)
	if err != nil {
		logger.Warn("load ML model; scoring stays disabled until one loads", "path", path, "error", err)
		return
	}
	guard.SetModel(model)
	metrics.MLModelInfo.Reset()
	metrics.MLModelInfo.WithLabelValues(model.Version).Set(1)
	logger.Info("ML model loaded", "path", path, "version", model.Version, "trees", len(model.Trees))
}
