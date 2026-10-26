//go:build !unix

package blobcache

import "os"

// lockFile does nothing where flock is unavailable; there concurrent processes sharing a cache can
// still collide on a partial file, which the final digest check catches and retries once.
func lockFile(*os.File) error {
	return nil
}
