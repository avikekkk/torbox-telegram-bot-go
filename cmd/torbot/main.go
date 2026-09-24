// Command torbot runs the TorBox Telegram bot.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/bot"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/config"
)

// logFile mirrors bot.LogFilePath, which the /logs command uploads.
const logFile = bot.LogFilePath

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	logger, closeLogs, err := setupLogging()
	if err != nil {
		return err
	}
	defer closeLogs()

	cfg, err := config.Load()
	if err != nil {
		var configErr *config.Error
		if errors.As(err, &configErr) {
			return fmt.Errorf("Configuration error: %w", err)
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	absLog, _ := filepath.Abs(logFile)
	logger.Info("Logging to " + absLog)

	err = bot.Run(ctx, cfg, logger)
	logger.Info("Bot stopped")
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// setupLogging writes to the console and to a log file that starts fresh on
// every run, so /logs uploads only the current session, as the usenet bot does.
func setupLogging() (*slog.Logger, func(), error) {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}

	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}

	handler := slog.NewTextHandler(io.MultiWriter(os.Stdout, file), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	return logger, func() { _ = file.Close() }, nil
}
