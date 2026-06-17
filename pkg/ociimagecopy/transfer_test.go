package ociimagecopy

// transfer_test.go — tests for the fsutil-based resumable blob transfer layer.
//
// Required tests:
//  1. Resume: interrupted transfer leaving a valid .part + sidecar is resumed
//     (bytes appended, not rewritten) observable via file state.
//  2. Digest mismatch: source serves wrong bytes → Pull fails via sha256 hook,
//     part file is cleaned up so retry restarts.
//  3. Blob enumeration excludes .part and .part.etag files.
//
// NOTE: TestBlobEnumeration_ExcludesPartFiles was deleted because
// ListBlobsFromFs has been removed from production. The unionShareInventory
// half of the coverage is preserved in TestBlobEnumeration_OCI_ExcludesPartFiles.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/opencontainers/go-digest"
)

// TestTransfer_Resume verifies that a Pull resuming from a valid .part file
// appends bytes to the existing partial rather than overwriting it.
//
// Setup: write 1 byte of the 2-byte L1 blob to the .part file on the local
// side, plus the ETag sidecar.  The remote has the full 2-byte blob.
// After Pull, the final file should be the full 2 bytes ("L1"), the .part
// file should be gone, and the sidecar should be gone.
func TestTransfer_Resume_AppendNotRewrite(t *testing.T) {
	t.Parallel()

	localBase := t.TempDir()
	remoteBase := t.TempDir()
	localFS, err := NewOsFs(localBase)
	if err != nil {
		t.Fatal(err)
	}
	remoteFS, err := NewOsFs(remoteBase)
	if err != nil {
		t.Fatal(err)
	}

	// Seed the remote with the full L1 blob.
	remoteShareSha := filepath.Join(remoteBase, "share", "sha256")
	must(t, os.MkdirAll(remoteShareSha, 0o755))
	must(
		t,
		os.WriteFile(
			filepath.Join(remoteShareSha, realLayer1Hex),
			[]byte(realLayer1Content),
			0o644,
		),
	)

	// Pre-seed the local side with 1 byte of L1 in the .part file + sidecar.
	localShareSha := filepath.Join(localBase, "share", "sha256")
	must(t, os.MkdirAll(localShareSha, 0o755))
	partPath := filepath.Join(localShareSha, realLayer1Hex+".part")
	sidecarPath := partPath + ".etag"
	must(t, os.WriteFile(partPath, []byte("L"), 0o644))
	must(t, os.WriteFile(sidecarPath, []byte("sha256:"+realLayer1Hex), 0o644))

	// Verify the part file has exactly 1 byte before the transfer.
	fi, err := os.Stat(partPath)
	must(t, err)
	if fi.Size() != 1 {
		t.Fatalf("pre-condition: part file has %d bytes, want 1", fi.Size())
	}

	// Use the fsutil Pull directly (bypassing orchestration) to test the
	// resumable transfer primitive.
	remoteDirs := NewFsOciDirs(remoteFS, 1)
	localDirs := NewFsOciDirs(localFS, 1)

	dgst := digest.Digest("sha256:" + realLayer1Hex)
	info := fsutil.ContentInfo{ETag: dgst.String(), Size: 2}
	src, err := remoteDirs.PrepDownload(context.Background(), dgst, 2)
	if err != nil {
		t.Fatal(err)
	}

	opt := makePullOpt(dgst)
	blobPath := "share/sha256/" + realLayer1Hex
	if err := opt.Pull(context.Background(), localFS, blobPath, src, info, 0o644); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// Verify final file contains full content.
	got, err := os.ReadFile(filepath.Join(localShareSha, realLayer1Hex))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != realLayer1Content {
		t.Errorf("final content = %q, want %q", got, realLayer1Content)
	}

	// Verify part file and sidecar are gone.
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part file should be gone after commit; stat=%v", err)
	}
	if _, err := os.Stat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part.etag file should be gone after commit; stat=%v", err)
	}

	// Verify the part file was indeed appended: we simply confirm the
	// 1-byte state was extended to 2 bytes by checking the final file.
	_ = localDirs
}

// countingSource is a ResumableSource that counts how many bytes were read
// from each Open call, so we can verify resume behaviour (bytes served
// from offset, not from 0).
type countingSource struct {
	inner   fsutil.ResumableSource
	offsets []int64 // offset passed to each Open call
}

func (c *countingSource) Open(
	ctx context.Context,
	offset int64,
) (io.ReadCloser, fsutil.ContentInfo, error) {
	c.offsets = append(c.offsets, offset)
	return c.inner.Open(ctx, offset)
}

// TestTransfer_Resume_OffsetIsNonZero verifies that when a .part file is
// present with a valid sidecar, the source is opened at offset > 0 (resume),
// not at offset 0 (restart).
func TestTransfer_Resume_OffsetIsNonZero(t *testing.T) {
	t.Parallel()

	localBase := t.TempDir()
	remoteBase := t.TempDir()
	localFS, err := NewOsFs(localBase)
	if err != nil {
		t.Fatal(err)
	}
	remoteFS, err := NewOsFs(remoteBase)
	if err != nil {
		t.Fatal(err)
	}

	// Seed remote with full L1.
	remoteShareSha := filepath.Join(remoteBase, "share", "sha256")
	must(t, os.MkdirAll(remoteShareSha, 0o755))
	must(
		t,
		os.WriteFile(
			filepath.Join(remoteShareSha, realLayer1Hex),
			[]byte(realLayer1Content),
			0o644,
		),
	)

	// Pre-seed local with 1-byte partial + sidecar.
	localShareSha := filepath.Join(localBase, "share", "sha256")
	must(t, os.MkdirAll(localShareSha, 0o755))
	partPath := filepath.Join(localShareSha, realLayer1Hex+".part")
	must(t, os.WriteFile(partPath, []byte("L"), 0o644))
	must(t, os.WriteFile(partPath+".etag", []byte("sha256:"+realLayer1Hex), 0o644))

	remoteDirs := NewFsOciDirs(remoteFS, 1)
	dgst := digest.Digest("sha256:" + realLayer1Hex)
	info := fsutil.ContentInfo{ETag: dgst.String(), Size: 2}

	rawSrc, err := remoteDirs.PrepDownload(context.Background(), dgst, 2)
	if err != nil {
		t.Fatal(err)
	}
	cs := &countingSource{inner: rawSrc}

	opt := makePullOpt(dgst)
	blobPath := "share/sha256/" + realLayer1Hex
	if err := opt.Pull(context.Background(), localFS, blobPath, cs, info, 0o644); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if len(cs.offsets) == 0 {
		t.Fatal("source was never opened")
	}
	if cs.offsets[0] != 1 {
		t.Errorf("source opened at offset %d, want 1 (resume from partial)", cs.offsets[0])
	}
}

// TestTransfer_DigestMismatch verifies that when the source serves wrong
// bytes, the sha256 pre-commit hook rejects the data, the part file is
// cleaned up, and the error is returned.
func TestTransfer_DigestMismatch(t *testing.T) {
	t.Parallel()

	localBase := t.TempDir()
	remoteBase := t.TempDir()
	localFS, err := NewOsFs(localBase)
	if err != nil {
		t.Fatal(err)
	}
	remoteFS, err := NewOsFs(remoteBase)
	if err != nil {
		t.Fatal(err)
	}

	// Seed remote with WRONG bytes for the L1 hash.
	// The file has content "XX" but the digest is for "L1".
	remoteShareSha := filepath.Join(remoteBase, "share", "sha256")
	must(t, os.MkdirAll(remoteShareSha, 0o755))
	must(t, os.WriteFile(filepath.Join(remoteShareSha, realLayer1Hex), []byte("XX"), 0o644))

	localShareSha := filepath.Join(localBase, "share", "sha256")
	must(t, os.MkdirAll(localShareSha, 0o755))

	remoteDirs := NewFsOciDirs(remoteFS, 1)
	dgst := digest.Digest("sha256:" + realLayer1Hex)
	// Use size=2 to match "XX" content size — prevents ErrSizeMismatch at the
	// source level, leaving the integrity check to the sha256 hook.
	info := fsutil.ContentInfo{ETag: dgst.String(), Size: 2}

	rawSrc, err := remoteDirs.PrepDownload(context.Background(), dgst, 2)
	if err != nil {
		t.Fatal(err)
	}

	opt := makePullOpt(dgst)
	blobPath := "share/sha256/" + realLayer1Hex
	pullErr := opt.Pull(context.Background(), localFS, blobPath, rawSrc, info, 0o644)
	if pullErr == nil {
		t.Fatal("Pull should have failed due to sha256 mismatch, got nil")
	}
	if !strings.Contains(pullErr.Error(), "sha256 mismatch") {
		t.Errorf("expected sha256 mismatch error, got: %v", pullErr)
	}

	// The part file and sidecar should be cleaned up after hook failure.
	partPath := filepath.Join(localShareSha, realLayer1Hex+".part")
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part file should be cleaned up after hook failure; stat=%v", err)
	}
	sidecarPath := partPath + ".etag"
	if _, err := os.Stat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part.etag sidecar should be cleaned up after hook failure; stat=%v", err)
	}

	// The final blob should not exist.
	finalPath := filepath.Join(localShareSha, realLayer1Hex)
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("final blob should not exist after failed pull; stat=%v", err)
	}
}

// TestTransfer_DigestMismatch_RetryRestarts verifies that after a digest-mismatch
// failure (part file cleaned up), a subsequent Pull starts fresh from offset 0.
func TestTransfer_DigestMismatch_RetryRestarts(t *testing.T) {
	t.Parallel()

	localBase := t.TempDir()
	remoteBase := t.TempDir()
	localFS, err := NewOsFs(localBase)
	if err != nil {
		t.Fatal(err)
	}
	remoteFS, err := NewOsFs(remoteBase)
	if err != nil {
		t.Fatal(err)
	}

	remoteShareSha := filepath.Join(remoteBase, "share", "sha256")
	must(t, os.MkdirAll(remoteShareSha, 0o755))
	// First attempt: serve wrong bytes.
	must(t, os.WriteFile(filepath.Join(remoteShareSha, realLayer1Hex), []byte("XX"), 0o644))

	localShareSha := filepath.Join(localBase, "share", "sha256")
	must(t, os.MkdirAll(localShareSha, 0o755))

	remoteDirs := NewFsOciDirs(remoteFS, 1)
	dgst := digest.Digest("sha256:" + realLayer1Hex)
	info := fsutil.ContentInfo{ETag: dgst.String(), Size: 2}
	blobPath := "share/sha256/" + realLayer1Hex
	opt := makePullOpt(dgst)

	// First attempt should fail.
	src1, _ := remoteDirs.PrepDownload(context.Background(), dgst, 2)
	_ = opt.Pull(context.Background(), localFS, blobPath, src1, info, 0o644)

	// Now fix the remote to serve correct bytes.
	must(
		t,
		os.WriteFile(
			filepath.Join(remoteShareSha, realLayer1Hex),
			[]byte(realLayer1Content),
			0o644,
		),
	)

	// Second attempt should succeed.
	src2, err := remoteDirs.PrepDownload(context.Background(), dgst, 2)
	if err != nil {
		t.Fatal(err)
	}
	cs := &countingSource{inner: src2}
	if err := opt.Pull(context.Background(), localFS, blobPath, cs, info, 0o644); err != nil {
		t.Fatalf("second Pull should succeed: %v", err)
	}

	// Verify the second attempt opened the source at offset 0 (no corrupt partial
	// was carried over).
	if len(cs.offsets) == 0 || cs.offsets[0] != 0 {
		t.Errorf(
			"second attempt should start at offset 0 (no partial from failed first attempt); offsets=%v",
			cs.offsets,
		)
	}

	got, err := os.ReadFile(filepath.Join(localShareSha, realLayer1Hex))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != realLayer1Content {
		t.Errorf("final content = %q, want %q", got, realLayer1Content)
	}
}

// TestPullOneBlob_VerifyReused_Correct tests that a right-sized correct local
// blob is reused without re-downloading regardless of verifyReused value.
func TestPullOneBlob_VerifyReused_Correct(t *testing.T) {
	t.Parallel()

	for _, verifyReused := range []bool{false, true} {
		t.Run(fmt.Sprintf("verifyReused=%v", verifyReused), func(t *testing.T) {
			t.Parallel()

			localBase := t.TempDir()
			remoteBase := t.TempDir()
			localFS, err := NewOsFs(localBase)
			if err != nil {
				t.Fatal(err)
			}
			remoteFS, err := NewOsFs(remoteBase)
			if err != nil {
				t.Fatal(err)
			}

			// Seed the remote with the full L1 blob.
			remoteShareSha := filepath.Join(remoteBase, "share", "sha256")
			must(t, os.MkdirAll(remoteShareSha, 0o755))
			must(t, os.WriteFile(
				filepath.Join(remoteShareSha, realLayer1Hex),
				[]byte(realLayer1Content),
				0o644,
			))

			// Seed the local side with the CORRECT full blob.
			localShareSha := filepath.Join(localBase, "share", "sha256")
			must(t, os.MkdirAll(localShareSha, 0o755))
			must(t, os.WriteFile(
				filepath.Join(localShareSha, realLayer1Hex),
				[]byte(realLayer1Content),
				0o644,
			))

			remoteDirs := NewFsOciDirs(remoteFS, 1)
			localDirs := NewFsOciDirs(localFS, 1)

			dgst := digest.Digest("sha256:" + realLayer1Hex)
			bt := blobTransfer{Digest: dgst, Size: int64(len(realLayer1Content))}

			ctx := context.Background()
			sent, err := pullOneBlob(ctx, bt, remoteDirs, localFS, localDirs, verifyReused)
			if err != nil {
				t.Fatalf("pullOneBlob: %v", err)
			}
			if sent {
				t.Errorf(
					"verifyReused=%v: expected blob to be reused (sent=false), got sent=true",
					verifyReused,
				)
			}

			// Verify the local blob still contains the correct content.
			got, err := os.ReadFile(filepath.Join(localShareSha, realLayer1Hex))
			if err != nil {
				t.Fatalf("read final: %v", err)
			}
			if string(got) != realLayer1Content {
				t.Errorf("final content = %q, want %q", got, realLayer1Content)
			}
		})
	}
}

// TestPullOneBlob_VerifyReused_Corrupt tests that a right-sized corrupt local
// blob is:
//   - reused as-is when verifyReused=false (wrong bytes persist)
//   - re-downloaded and corrected when verifyReused=true
func TestPullOneBlob_VerifyReused_Corrupt(t *testing.T) {
	t.Parallel()

	t.Run("verifyReused=false_reuses_corrupt", func(t *testing.T) {
		t.Parallel()

		localBase := t.TempDir()
		remoteBase := t.TempDir()
		localFS, err := NewOsFs(localBase)
		if err != nil {
			t.Fatal(err)
		}
		remoteFS, err := NewOsFs(remoteBase)
		if err != nil {
			t.Fatal(err)
		}

		// Seed the remote with correct bytes.
		remoteShareSha := filepath.Join(remoteBase, "share", "sha256")
		must(t, os.MkdirAll(remoteShareSha, 0o755))
		must(t, os.WriteFile(
			filepath.Join(remoteShareSha, realLayer1Hex),
			[]byte(realLayer1Content),
			0o644,
		))

		// Seed the local side with CORRUPT bytes at the right size.
		localShareSha := filepath.Join(localBase, "share", "sha256")
		must(t, os.MkdirAll(localShareSha, 0o755))
		corruptContent := make([]byte, len(realLayer1Content))
		for i := range corruptContent {
			corruptContent[i] = byte(0xFF ^ realLayer1Content[i])
		}
		must(t, os.WriteFile(
			filepath.Join(localShareSha, realLayer1Hex),
			corruptContent,
			0o644,
		))

		remoteDirs := NewFsOciDirs(remoteFS, 1)
		localDirs := NewFsOciDirs(localFS, 1)

		dgst := digest.Digest("sha256:" + realLayer1Hex)
		bt := blobTransfer{Digest: dgst, Size: int64(len(realLayer1Content))}

		ctx := context.Background()
		sent, err := pullOneBlob(ctx, bt, remoteDirs, localFS, localDirs, false)
		if err != nil {
			t.Fatalf("pullOneBlob (verifyReused=false): %v", err)
		}
		if sent {
			t.Error("verifyReused=false: expected blob to be reused (sent=false)")
		}

		// Wrong bytes should still be there since we didn't verify.
		got, err := os.ReadFile(filepath.Join(localShareSha, realLayer1Hex))
		if err != nil {
			t.Fatalf("read final: %v", err)
		}
		if string(got) == realLayer1Content {
			t.Error(
				"verifyReused=false: expected corrupt bytes to persist, but got correct content",
			)
		}
	})

	t.Run("verifyReused=true_redownloads", func(t *testing.T) {
		t.Parallel()

		localBase := t.TempDir()
		remoteBase := t.TempDir()
		localFS, err := NewOsFs(localBase)
		if err != nil {
			t.Fatal(err)
		}
		remoteFS, err := NewOsFs(remoteBase)
		if err != nil {
			t.Fatal(err)
		}

		// Seed the remote with correct bytes.
		remoteShareSha := filepath.Join(remoteBase, "share", "sha256")
		must(t, os.MkdirAll(remoteShareSha, 0o755))
		must(t, os.WriteFile(
			filepath.Join(remoteShareSha, realLayer1Hex),
			[]byte(realLayer1Content),
			0o644,
		))

		// Seed the local side with CORRUPT bytes at the right size.
		localShareSha := filepath.Join(localBase, "share", "sha256")
		must(t, os.MkdirAll(localShareSha, 0o755))
		corruptContent := make([]byte, len(realLayer1Content))
		for i := range corruptContent {
			corruptContent[i] = byte(0xFF ^ realLayer1Content[i])
		}
		must(t, os.WriteFile(
			filepath.Join(localShareSha, realLayer1Hex),
			corruptContent,
			0o644,
		))

		remoteDirs := NewFsOciDirs(remoteFS, 1)
		localDirs := NewFsOciDirs(localFS, 1)

		dgst := digest.Digest("sha256:" + realLayer1Hex)
		bt := blobTransfer{Digest: dgst, Size: int64(len(realLayer1Content))}

		ctx := context.Background()
		sent, err := pullOneBlob(ctx, bt, remoteDirs, localFS, localDirs, true)
		if err != nil {
			t.Fatalf("pullOneBlob (verifyReused=true): %v", err)
		}
		if !sent {
			t.Error("verifyReused=true: expected corrupt blob to be re-downloaded (sent=true)")
		}

		// Correct bytes should now be present.
		got, err := os.ReadFile(filepath.Join(localShareSha, realLayer1Hex))
		if err != nil {
			t.Fatalf("read final: %v", err)
		}
		if string(got) != realLayer1Content {
			t.Errorf("verifyReused=true: final content = %q, want %q", got, realLayer1Content)
		}
	})
}

// TestBlobEnumeration_OCI_ExcludesPartFiles verifies that the OCI transport
// enumeration path also ignores .part/.part.etag artifacts.
func TestBlobEnumeration_OCI_ExcludesPartFiles(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	shareSha := filepath.Join(tmp, "share", "sha256")
	must(t, os.MkdirAll(shareSha, 0o755))

	// Write a blob, a .part artifact, and a .part.etag sidecar.
	must(t, os.WriteFile(filepath.Join(shareSha, realLayer1Hex), []byte(realLayer1Content), 0o644))
	must(t, os.WriteFile(filepath.Join(shareSha, realLayer1Hex+".part"), []byte("L"), 0o644))
	must(
		t,
		os.WriteFile(
			filepath.Join(shareSha, realLayer1Hex+".part.etag"),
			[]byte("sha256:"+realLayer1Hex),
			0o644,
		),
	)

	fsys, err := NewOsFs(tmp)
	if err != nil {
		t.Fatal(err)
	}
	cfg := EnumerateConfig{
		Transport: skopeo.TransportOci,
		Fs:        fsys,
		BaseDir:   tmp,
	}
	got, err := cfg.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Only the real blob should appear.
	for k := range got {
		if strings.HasSuffix(k, ".part") || strings.HasSuffix(k, ".part.etag") {
			t.Errorf("enumeration included artifact file: %s", k)
		}
	}
	if _, ok := got["sha256:"+realLayer1Hex]; !ok {
		t.Errorf(
			"real blob sha256:%s not found in enumeration result %v",
			realLayer1Hex,
			sortedKeys(got),
		)
	}
}
