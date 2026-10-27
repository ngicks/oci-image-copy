package cmdsignals

import (
	"context"
	"errors"
	"testing"
	"time"
)

const testTimeout = 2 * time.Second

// startNotifier calls NotifyContext, runs the Notifier, and returns a channel
// closed when Run returns. Cleanup performs the advised deferred cancel +
// Stop so the process-global signal registration does not leak into other
// tests.
func startNotifier(t *testing.T) (
	n *Notifier,
	ctx context.Context,
	cancel context.CancelCauseFunc,
	done <-chan struct{},
) {
	t.Helper()
	n, ctx, cancel = NotifyContext(context.Background())
	d := make(chan struct{})
	go func() {
		n.Run()
		close(d)
	}()
	t.Cleanup(func() {
		cancel(nil)
		n.Stop()
		<-d
	})
	return n, ctx, cancel, d
}

func mustReturn(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal(msg)
	}
}

// TestNotifyContext_CancelPreservesCauseAndStopEndsRun asserts the caller's
// own cancel(err) preserves its cause, that cancel alone does not end Run
// (stopping is the caller's deferred Stop), and that Stop makes Run return.
func TestNotifyContext_CancelPreservesCauseAndStopEndsRun(t *testing.T) {
	n, ctx, cancel, done := startNotifier(t)

	want := errors.New("shutdown")
	cancel(want)

	if got := context.Cause(ctx); got != want {
		t.Fatalf("cause = %v, want %v", got, want)
	}

	// Cancelling must not end Run by itself; this can only fail when Run has
	// genuinely returned already.
	select {
	case <-done:
		t.Fatal("Run returned on cancel alone, before Stop was called")
	case <-time.After(10 * time.Millisecond):
	}

	n.Stop()
	mustReturn(t, done, "Run did not return after Stop")
}

func TestNotifyContext_StoresNotifierInContext(t *testing.T) {
	n, ctx, _, _ := startNotifier(t)

	got, ok := CtxValueNotifier(ctx)
	if !ok || got != n {
		t.Fatalf("CtxValueNotifier = (%v, %v), want the returned Notifier", got, ok)
	}
	if _, ok := CtxValueNotifier(context.Background()); ok {
		t.Fatal("CtxValueNotifier returned true for a context without a Notifier")
	}
}
