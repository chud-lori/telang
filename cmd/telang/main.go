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
	"syscall"
	"time"

	"github.com/telang/telang/internal/config"
	"github.com/telang/telang/internal/metadata"
	"github.com/telang/telang/internal/s3api"
	"github.com/telang/telang/internal/sigv4"
	"github.com/telang/telang/internal/storage"
	"github.com/telang/telang/internal/storage/bot"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "telang serve:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "telang — S3-compatible object storage backed by Telegram")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  telang serve --config /path/to/config.toml")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/telang/config.toml", "path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Ensure staging and DB dirs exist (cache dir is created lazily in v0.2).
	for _, dir := range []string{filepath.Dir(cfg.Storage.DBPath), cfg.Storage.StagingDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	meta, err := metadata.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	defer meta.Close()

	backend, err := buildBackend(cfg)
	if err != nil {
		return err
	}

	svc := &s3api.Service{
		Meta:       meta,
		Backend:    backend,
		StagingDir: cfg.Storage.StagingDir,
	}

	verifier := &sigv4.Verifier{
		Region: cfg.S3.Region,
		Lookup: func(ak string) (string, bool) {
			if ak == cfg.S3.AccessKey {
				return cfg.S3.SecretKey, true
			}
			return "", false
		},
	}

	handler := &s3api.Handler{
		Verifier: verifier,
		Service:  svc,
		Logger:   logger,
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("telang serving", "addr", cfg.Server.Listen, "mode", cfg.Telegram.Mode)
		err := srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errCh
}

func buildBackend(cfg *config.Config) (storage.Backend, error) {
	switch cfg.Telegram.Mode {
	case config.ModeBot:
		return bot.New(cfg.Telegram.BotToken, cfg.Telegram.ChannelID)
	case config.ModeMTProto:
		return nil, errors.New("telegram mode \"mtproto\" is not yet implemented (v0.3)")
	default:
		return nil, fmt.Errorf("telegram mode %q is not supported", cfg.Telegram.Mode)
	}
}
