//go:build unix

package cmdsignals

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestNotifyContext_DeliversRealSignal exercises the full wiring by sending
// the process a real SIGTERM and asserting the returned context is cancelled
// with the matching cause.
func TestNotifyContext_DeliversRealSignal(t *testing.T) {
	_, ctx, _, _ := startNotifier(t)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("failed to send SIGTERM: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after SIGTERM")
	}

	want := fmt.Sprintf("signal received: %q", syscall.SIGTERM)
	if got := context.Cause(ctx); got == nil || got.Error() != want {
		t.Fatalf("cause = %v, want %q", got, want)
	}
	// The cause must be recoverable through the re-exported alias.
	sigErr, ok := errors.AsType[*SignalReceivedError](context.Cause(ctx))
	if !ok || sigErr.Sig != syscall.SIGTERM {
		t.Fatalf("AsType[*SignalReceivedError] = (%v, %v), want SIGTERM", sigErr, ok)
	}
}

// TestNotifierSwap_DivertsRealSignal asserts that after Notifier.Swap a real
// SIGTERM reaches the swapped-in handler instead of cancelling the context,
// and that Restore brings back cancellation with the signal cause.
func TestNotifierSwap_DivertsRealSignal(t *testing.T) {
	n, ctx, _, _ := startNotifier(t)

	forward := make(chan os.Signal, 1)
	h := NewHandler(1, func(sig os.Signal) { forward <- sig }, nil)
	go h.Run()
	defer h.Stop()
	n.Swap(h)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("failed to send SIGTERM: %v", err)
	}

	select {
	case sig := <-forward:
		if sig != syscall.SIGTERM {
			t.Fatalf("diverted signal = %v, want %v", sig, syscall.SIGTERM)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("swapped-in handler did not receive the signal")
	}
	if ctx.Err() != nil {
		t.Fatalf("context was cancelled while diverted: %v", context.Cause(ctx))
	}

	n.Restore()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("failed to send SIGTERM: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after SIGTERM once restored")
	}

	want := fmt.Sprintf("signal received: %q", syscall.SIGTERM)
	if got := context.Cause(ctx); got == nil || got.Error() != want {
		t.Fatalf("cause = %v, want %q", got, want)
	}
}
