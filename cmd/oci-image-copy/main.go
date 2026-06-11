package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/oci-image-copy/cmd/internal/cmdsignals"
	"github.com/ngicks/oci-image-copy/cmd/oci-image-copy/commands"
)

func main() {
	logger := slog.New(
		slog.NewJSONHandler(
			os.Stdout,
			&slog.HandlerOptions{
				AddSource: true,
				Level:     slog.LevelDebug,
			},
		),
	)

	ctx, stop := signal.NotifyContext(
		context.Background(),
		cmdsignals.ExitSignals[:]...,
	)
	defer stop()

	ctx = contextkey.WithSlogLogger(ctx, logger)

	if err := commands.Execute(ctx); err != nil {
		logger.ErrorContext(ctx, "stopped with an error", slog.Any("err", err))
		os.Exit(1)
	}
}
