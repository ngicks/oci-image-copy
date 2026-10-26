// Command storecheck opens a containers-storage store and wraps it with cstorage, then reports what
// it holds. It exists so the justfile build targets can be checked against a real program that
// links cstorage.
//
//	storecheck -root DIR [-driver vfs]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"

	"github.com/ngicks/oci-image-copy/blobcache"
	"github.com/ngicks/oci-image-copy/cstorage"
)

func main() {
	if reexec.Init() {
		return
	}
	root := flag.String("root", "", "directory holding graph/, run/ and cache/")
	driver := flag.String("driver", "vfs", "graph driver")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "storecheck: -root is required")
		os.Exit(2)
	}
	if err := run(*root, *driver); err != nil {
		fmt.Fprintln(os.Stderr, "storecheck:", err)
		os.Exit(1)
	}
}

func run(root, driver string) error {
	cache, err := blobcache.New(filepath.Join(root, "cache"))
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()

	st, err := storage.GetStore(storage.StoreOptions{
		GraphRoot:       filepath.Join(root, "graph"),
		RunRoot:         filepath.Join(root, "run"),
		GraphDriverName: driver,
	})
	if err != nil {
		return err
	}
	defer func() { _, _ = st.Shutdown(false) }()
	cs := cstorage.New(st, cache, nil)
	defer func() { _ = cs.Close() }()

	layers, err := st.Layers()
	if err != nil {
		return err
	}
	images, err := st.Images()
	if err != nil {
		return err
	}
	fmt.Printf("driver=%s layers=%d images=%d\n", st.GraphDriverName(), len(layers), len(images))
	return nil
}
