package commands

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

// TestRootCmd_LoggingEnabledByDefault locks in this project's deliberate
// relaxation of loggerfactory's opt-in default: with no --log/--log-level flag
// and no OCI_IMAGE_COPY_LOG_* env var, logging must still be enabled. The root
// command's PersistentPreRun runs for any subcommand (here "version"), and a
// disabled logger is backed by slog.DiscardHandler whose Enabled always reports
// false — so asserting Enabled(info)==true distinguishes on from off.
func TestRootCmd_LoggingEnabledByDefault(t *testing.T) {
	// PersistentPreRun calls slog.SetDefault; save and restore the global so
	// this test does not leak an enabled logger into other tests.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	cmd := rootCmd()
	cmd.SetArgs([]string{"version"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version: %v", err)
	}

	if !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("expected logging enabled by default (info), got a disabled/discard logger")
	}
}
