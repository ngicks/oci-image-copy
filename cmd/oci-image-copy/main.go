package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"go.podman.io/storage/pkg/reexec"

	"github.com/ngicks/oci-image-copy/cmd/oci-image-copy/commands"
	"github.com/ngicks/oci-image-copy/internal/cmdsignals"
)

func main() {
	// containers-storage re-executes this binary for chroot-ed layer extraction; the child
	// must branch off before any flag parsing or signal setup.
	if reexec.Init() {
		return
	}

	n, ctx, cancel := cmdsignals.NotifyContext(context.Background())

	var wg sync.WaitGroup
	wg.Go(n.Run)

	err := commands.Execute(ctx)

	// Check before cancel(nil) below — that call would set ctx.Err() and
	// manufacture a false positive.
	if err != nil && errors.Is(err, ctx.Err()) {
		if sigErr, ok := errors.AsType[*cmdsignals.SignalReceivedError](context.Cause(ctx)); ok {
			err = sigErr
		}
	}

	cancel(nil)
	n.Stop()
	wg.Wait()

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
