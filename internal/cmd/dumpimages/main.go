// Command dumpimages populates internal/testdata/ocidir/ with real
// OCI dumps so the orchestrator's `_Local` tests have something to
// chew on. It drives [pkg/cli/skopeo] (i.e. the system `skopeo`
// binary) to write the per-image entries into one shared blob pool
// at internal/testdata/ocidir/share + one OCI dir at
// internal/testdata/ocidir/image. Run from the repo root:
//
//	go run ./internal/cmd/dumpimages
//
// Sources use the `docker://` transport, so the dumper pulls each
// entry straight from its registry — no local pull (podman/docker)
// required. Add new entries to the `images` slice below.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const (
	storePath = "internal/testdata/ocidir"
	donePath  = "internal/testdata/ocidir/done"
)

var images = []string{
	// same image
	"gcr.io/distroless/base-debian12" +
		"@sha256:9dce90e688a57e59ce473ff7bc4c80bc8fe52d2303b4d99b44f297310bbd2210",
	"docker.io/library/memcached:1.6.41",
	// same image
	"docker.io/library/memcached" +
		"@sha256:277e0c4f249b118e95ab10e535bae2fa1af772271d9152f3468e58d59348db56",
	"docker.io/library/memcached:1.5.22",
	"docker.io/library/memcached" +
		"@sha256:12f7570b87465bdc8104c54389d6dbecff01270fd76b3853dd78a1937dd5d6c8",
}

func main() {
	flag.Parse()

	if _, err := os.Stat(donePath); err == nil {
		log.Printf("done flag file; if regeneration is needed, remove %q", donePath)
		return
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(donePath)
	if err == nil {
		_ = f.Close()
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		"go",
		append([]string{
			"run",
			"./cmd/oci-image-copy",
			"dump",
			"--local-dumpdir", storePath,
			"--local-transport", "docker",
		},
			images...,
		)...,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
