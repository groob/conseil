package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultInstructions = `You are Conseil, a personal assistant for one user. Be concise and direct. State uncertainty plainly. Never claim an action succeeded unless its result confirms it.`

type config struct {
	addr                 string
	databasePath         string
	llmBaseURL           string
	agentName            string
	model                string
	reasoningEffort      string
	instructions         string
	ownerEmail           string
	allowUnauthenticated bool
	runTimeout           time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("loading configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, logger); err != nil {
		logger.Error("running conseil", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	allowUnauthenticated, err := strconv.ParseBool(envOr("CONSEIL_ALLOW_UNAUTHENTICATED", "false"))
	if err != nil {
		return config{}, fmt.Errorf("parsing CONSEIL_ALLOW_UNAUTHENTICATED: %w", err)
	}
	runTimeout, err := time.ParseDuration(envOr("CONSEIL_RUN_TIMEOUT", "10m"))
	if err != nil {
		return config{}, fmt.Errorf("parsing CONSEIL_RUN_TIMEOUT: %w", err)
	}
	if runTimeout <= 0 {
		return config{}, errors.New("CONSEIL_RUN_TIMEOUT must be positive")
	}

	cfg := config{
		addr:                 envOr("CONSEIL_ADDR", "127.0.0.1:8000"),
		databasePath:         envOr("CONSEIL_DB_PATH", "conseil.db"),
		llmBaseURL:           strings.TrimRight(envOr("CONSEIL_LLM_BASE_URL", "https://llm.int.exe.xyz/v1"), "/"),
		agentName:            envOr("CONSEIL_AGENT_NAME", "conseil"),
		model:                envOr("CONSEIL_MODEL", "gpt-5.6-sol"),
		reasoningEffort:      envOr("CONSEIL_REASONING_EFFORT", "high"),
		instructions:         envOr("CONSEIL_INSTRUCTIONS", defaultInstructions),
		ownerEmail:           strings.TrimSpace(os.Getenv("CONSEIL_OWNER_EMAIL")),
		allowUnauthenticated: allowUnauthenticated,
		runTimeout:           runTimeout,
	}
	if !cfg.allowUnauthenticated && cfg.ownerEmail == "" {
		return config{}, errors.New("CONSEIL_OWNER_EMAIL is required unless CONSEIL_ALLOW_UNAUTHENTICATED=true")
	}
	if err := validateBindAddress(cfg.addr, cfg.allowUnauthenticated); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func validateBindAddress(address string, allowUnauthenticated bool) error {
	if allowUnauthenticated {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parsing CONSEIL_ADDR: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("CONSEIL_ADDR must use a loopback address when authentication is enabled")
}

func serve(ctx context.Context, cfg config, logger *slog.Logger) error {
	store, err := openStore(cfg.databasePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.close(); err != nil {
			logger.Error("closing database", "error", err)
		}
	}()

	interrupted, err := store.recoverInterrupted(ctx)
	if err != nil {
		return fmt.Errorf("recovering interrupted runs: %w", err)
	}
	if len(interrupted) > 0 {
		logger.Warn("marked interrupted runs as failed", "count", len(interrupted))
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	runner := &llmAgent{
		baseURL:         cfg.llmBaseURL,
		model:           cfg.model,
		reasoningEffort: cfg.reasoningEffort,
		instructions:    cfg.instructions,
		client:          &http.Client{Transport: transport},
	}
	hub := newEventHub()
	worker := newWorker(store, runner, hub, logger, cfg.runTimeout)
	api := &api{
		store:                store,
		worker:               worker,
		hub:                  hub,
		logger:               logger,
		agentName:            cfg.agentName,
		model:                cfg.model,
		instructions:         cfg.instructions,
		ownerEmail:           cfg.ownerEmail,
		allowUnauthenticated: cfg.allowUnauthenticated,
	}
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           api.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fatal := make(chan error, 2)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		if err := worker.run(serviceCtx); err != nil {
			fatal <- fmt.Errorf("running worker: %w", err)
		}
	}()
	go func() {
		defer group.Done()
		logger.Info("listening", "address", cfg.addr, "model", cfg.model, "reasoning_effort", cfg.reasoningEffort)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal <- fmt.Errorf("serving HTTP: %w", err)
		}
	}()
	worker.notify()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-fatal:
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shutting down HTTP server: %w", err)
	}
	group.Wait()
	return runErr
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
