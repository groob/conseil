// Command vitrier-broker mints repository-scoped GitHub tokens for
// authenticated exe.dev peer VMs.
package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	brokerAddress      = "127.0.0.1:8000"
	brokerOrganization = "maintenancewindows"
	grantsDirectory    = "/etc/vitrier-broker/grants"
)

type config struct {
	appID          int64
	privateKeyPath string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	store := newGrantStore(grantsDirectory)
	if len(os.Args) > 1 {
		if err := runAdmin(os.Args[1:], store, os.Stdout); err != nil {
			logger.Error("running admin command", "error", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("loading configuration", "error", err)
		os.Exit(1)
	}
	privateKey, err := readPrivateKey(cfg.privateKeyPath)
	if err != nil {
		logger.Error("reading GitHub App private key", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, privateKey, store, logger); err != nil {
		logger.Error("running token broker", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	appID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("VITRIER_APP_ID")), 10, 64)
	if err != nil || appID <= 0 {
		return config{}, errors.New("VITRIER_APP_ID must be a positive integer")
	}
	credentialsDirectory := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY"))
	if credentialsDirectory == "" {
		return config{}, errors.New("CREDENTIALS_DIRECTORY is required")
	}
	return config{appID: appID, privateKeyPath: filepath.Join(credentialsDirectory, "github-app-key")}, nil
}

func validVMName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, character := range name {
		if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func serve(ctx context.Context, cfg config, privateKey *rsa.PrivateKey, store grantStore, logger *slog.Logger) error {
	client := &http.Client{Timeout: 30 * time.Second}
	minter := newGitHubMinter(cfg.appID, brokerOrganization, privateKey, client)
	api := &api{
		grants: store,
		minter: minter,
		logger: logger,
	}
	server := &http.Server{
		Addr:              brokerAddress,
		Handler:           api.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	result := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", brokerAddress, "organization", brokerOrganization, "grants_dir", grantsDirectory)
		result <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down HTTP server: %w", err)
		}
		err := <-result
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving HTTP: %w", err)
		}
		return nil
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving HTTP: %w", err)
	}
}
