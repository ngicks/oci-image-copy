//go:build unix

package blobcache

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, waiting until it is free; closing f releases it.
// flock locks belong to the open file, so two opens within one process contend as well.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
