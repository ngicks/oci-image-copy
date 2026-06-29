package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ngicks/oci-image-copy/cmd/internal/cmdsignals"
	"github.com/ngicks/oci-image-copy/cmd/oci-image-copy/commands"
)

func main() {
	blockOn, ctx, cancel := cmdsignals.NotifyContext(context.Background())

	// blockOn watches ExitSignals and cancels ctx when one arrives; it must run
	// for signal propagation to work, so start it before Execute. cancel + Wait
	// tear the goroutine down afterwards — whether Execute returned on its own or
	// because a signal already cancelled ctx (cancel is a no-op in that case).
	var wg sync.WaitGroup
	wg.Go(blockOn)

	err := commands.Execute(ctx)

	// Recover the cancellation reason while ctx still reflects it. The guard is
	// errors.Is(err, ctx.Err()) — not the bare context.Canceled sentinel, which
	// any code may return without this ctx being cancelled — so it fires only
	// when *this* context was actually cancelled. Read it before cancel(nil)
	// below, or that cleanup call would set ctx.Err() and manufacture a false
	// positive. Execute surfaces only context.Canceled; the signal lives in the
	// cause as *SignalReceivedError.
	if err != nil && errors.Is(err, ctx.Err()) {
		if sigErr, ok := errors.AsType[*cmdsignals.SignalReceivedError](context.Cause(ctx)); ok {
			err = sigErr
		}
	}

	cancel(nil)
	wg.Wait()

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
