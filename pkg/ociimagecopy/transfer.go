package ociimagecopy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

// CopyBufferSize is the io.CopyBuffer chunk size used when streaming
// blob bytes. Tuned for SFTP throughput (the kernel limits SFTP payloads
// to ~32 KiB anyway, but a larger buffer reduces syscall overhead on
// the read side).
const CopyBufferSize = 256 * 1024

// safeWriteOpt is the [fsutil.SafeWriteOption] used by
// [FsOciDirs.PutTagFile] for tmp + atomic-rename writes of small
// per-image metadata files.
var safeWriteOpt = fsutil.SafeWriteOption[vroot.Fs[vroot.File], vroot.File]{
	TempFilePolicy: fsutil.NewTempFilePolicyDir[vroot.Fs[vroot.File]]("__temp__"),
}

// blobCopyOpt is the [fsutil.ResumableCopyOption] used for blob push
// transfers (local → remote). No digest hook: push side trusts the
// local file (it was populated by a previous Pull that was
// hook-verified, or by skopeo which validates on import).
var blobCopyOpt = fsutil.ResumableCopyOption[vroot.Fs[vroot.File], vroot.File]{
	BufSize: CopyBufferSize,
}

// makePullOpt returns a [fsutil.ResumableCopyOption] with a sha256
// PreCommitHook that verifies the downloaded blob against dgst before
// the atomic rename. On hook failure the part file is removed so the
// next attempt restarts clean.
func makePullOpt(dgst digest.Digest) fsutil.ResumableCopyOption[vroot.Fs[vroot.File], vroot.File] {
	return fsutil.ResumableCopyOption[vroot.Fs[vroot.File], vroot.File]{
		BufSize: CopyBufferSize,
		PreCommitHooks: []func(vroot.File, string) error{
			func(f vroot.File, partPath string) error {
				verifier := dgst.Verifier()
				if _, err := io.Copy(verifier, f); err != nil {
					return fmt.Errorf("sha256 verify read %s: %w", partPath, err)
				}
				if !verifier.Verified() {
					return fmt.Errorf("sha256 mismatch for digest %s (part: %s)", dgst, partPath)
				}
				return nil
			},
		},
	}
}

// blobTransfer describes one blob to move between two OciDirs.
type blobTransfer struct {
	Digest digest.Digest
	Size   int64
}

// pullBlobs streams each blob from srcDirs (remote) to localFs / localDirs
// using the fsutil Pull primitive with sha256 verification.
// Concurrency is capped to parallelism.
// verifyReused controls whether right-sized existing blobs are sha256-checked
// before reuse; see [pullOneBlob] for details.
func pullBlobs(
	ctx context.Context,
	blobs []blobTransfer,
	srcDirs OciDirs,
	localFs vroot.Fs[vroot.File],
	localDirs OciDirs,
	parallelism int,
	verifyReused bool,
) (PutBlobsResult, error) {
	var (
		result PutBlobsResult
		mu     sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	if parallelism > 0 {
		g.SetLimit(parallelism)
	}

	for _, bt := range blobs {
		if gctx.Err() != nil {
			break
		}
		// capture
		g.Go(func() error {
			sent, err := pullOneBlob(gctx, bt, srcDirs, localFs, localDirs, verifyReused)
			if err != nil {
				return fmt.Errorf("blob %s: %w", bt.Digest, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if sent {
				result.Sent++
				result.BytesSent += bt.Size
			} else {
				result.Reused++
			}
			return nil
		})
	}
	return result, g.Wait()
}

// pullOneBlob downloads one blob from srcDirs into localFs at the blob's
// canonical share path, using the fsutil Pull primitive with sha256
// verification.
// Returns (true, nil) when bytes were written; (false, nil) when already complete.
//
// When verifyReused is true and a right-sized local file exists, the file is
// sha256-verified before reuse. A corrupt file (right size, wrong content) is
// removed so the download path re-fetches and re-verifies it.
func pullOneBlob(
	ctx context.Context,
	bt blobTransfer,
	srcDirs OciDirs,
	localFs vroot.Fs[vroot.File],
	localDirs OciDirs,
	verifyReused bool,
) (bool, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	info := fsutil.ContentInfo{ETag: bt.Digest.String(), Size: bt.Size}

	blobPath, err := RelBlobPath(string(bt.Digest))
	if err != nil {
		return false, err
	}

	// Check if already complete.
	if fi, err := localFs.Stat(blobPath); err == nil {
		if fi.Size() == bt.Size {
			if verifyReused {
				// Verify sha256 of existing file before reusing.
				f, err := localFs.OpenFile(blobPath, os.O_RDONLY, 0)
				if err == nil {
					verifier := bt.Digest.Verifier()
					_, copyErr := io.Copy(verifier, f)
					_ = f.Close()
					if copyErr == nil && verifier.Verified() {
						logger.LogAttrs(ctx, slog.LevelInfo, "transfer.pull.skip.verified",
							slog.String("blob", bt.Digest.String()),
							slog.Int64("size", fi.Size()),
						)
						return false, nil
					}
					// Mismatch or read error: remove and re-download.
					logger.LogAttrs(ctx, slog.LevelWarn, "transfer.pull.corrupt",
						slog.String("blob", bt.Digest.String()),
						slog.Int64("size", fi.Size()),
					)
					_ = localFs.Remove(blobPath)
				}
				// Fall through to download.
			} else {
				logger.LogAttrs(ctx, slog.LevelInfo, "transfer.pull.skip",
					slog.String("blob", bt.Digest.String()),
					slog.Int64("size", fi.Size()),
				)
				return false, nil
			}
		}
	}

	// Ensure parent dir exists.
	if err := localDirs.MkdirBlobParent(bt.Digest); err != nil {
		return false, fmt.Errorf("mkdir parent: %w", err)
	}

	src, err := srcDirs.BlobSource(ctx, bt.Digest, bt.Size)
	if err != nil {
		return false, fmt.Errorf("blob-source: %w", err)
	}
	opt := makePullOpt(bt.Digest)
	logger.LogAttrs(ctx, slog.LevelInfo, "transfer.pull",
		slog.String("blob", bt.Digest.String()),
		slog.Int64("size", bt.Size),
	)
	if err := opt.Pull(ctx, localFs, blobPath, src, info, 0o644); err != nil {
		return false, fmt.Errorf("pull blob: %w", err)
	}
	return true, nil
}

// pushBlobs streams each blob from localFs / localDirs to remoteDirs
// using the fsutil Push primitive. Concurrency is capped to parallelism.
func pushBlobs(
	ctx context.Context,
	blobs []blobTransfer,
	localFs vroot.Fs[vroot.File],
	remoteDirs OciDirs,
	parallelism int,
) (PutBlobsResult, error) {
	var (
		result PutBlobsResult
		mu     sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	if parallelism > 0 {
		g.SetLimit(parallelism)
	}

	for _, bt := range blobs {
		if gctx.Err() != nil {
			break
		}
		// capture
		g.Go(func() error {
			sent, err := pushOneBlob(gctx, bt, localFs, remoteDirs)
			if err != nil {
				return fmt.Errorf("blob %s: %w", bt.Digest, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if sent {
				result.Sent++
				result.BytesSent += bt.Size
			} else {
				result.Reused++
			}
			return nil
		})
	}
	return result, g.Wait()
}

// pushOneBlob uploads one blob from localFs to remoteDirs.
// Returns (true, nil) when bytes were written; (false, nil) when already complete.
func pushOneBlob(
	ctx context.Context,
	bt blobTransfer,
	localFs vroot.Fs[vroot.File],
	remoteDirs OciDirs,
) (bool, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	info := fsutil.ContentInfo{ETag: bt.Digest.String(), Size: bt.Size}

	blobPath, err := RelBlobPath(string(bt.Digest))
	if err != nil {
		return false, err
	}

	// Ensure parent dir exists on the remote side before writing.
	if err := remoteDirs.MkdirBlobParent(bt.Digest); err != nil {
		return false, fmt.Errorf("mkdir remote parent: %w", err)
	}

	sink, err := remoteDirs.BlobSink(ctx, bt.Digest, bt.Size)
	if err != nil {
		return false, fmt.Errorf("blob-sink: %w", err)
	}

	// Check if already complete via sink state.
	st, err := sink.State(ctx)
	if err != nil {
		return false, fmt.Errorf("sink state: %w", err)
	}
	if st.Complete {
		logger.LogAttrs(ctx, slog.LevelInfo, "transfer.push.skip",
			slog.String("blob", bt.Digest.String()),
			slog.Int64("size", bt.Size),
		)
		return false, nil
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "transfer.push",
		slog.String("blob", bt.Digest.String()),
		slog.Int64("size", bt.Size),
	)
	if err := blobCopyOpt.Push(ctx, localFs, blobPath, sink, info); err != nil {
		return false, fmt.Errorf("push blob: %w", err)
	}
	return true, nil
}

// descriptorDigestSet returns the digest set of every descriptor.
func descriptorDigestSet(descs []v1.Descriptor) map[string]struct{} {
	out := make(map[string]struct{}, len(descs))
	for _, d := range descs {
		out[string(d.Digest)] = struct{}{}
	}
	return out
}

// descriptorSizes returns the digest→size map for every descriptor
// with a non-zero Size. Descriptors with Size == 0 are omitted (size
// is not authoritative for them).
func descriptorSizes(descs []v1.Descriptor) map[string]int64 {
	out := make(map[string]int64, len(descs))
	for _, d := range descs {
		if d.Size > 0 {
			out[string(d.Digest)] = d.Size
		}
	}
	return out
}

// sortedDigests returns ds in lexical order so transfer scheduling is
// deterministic (helps with test assertions and log readability).
func sortedDigests(ds map[string]struct{}) []string {
	out := make([]string, 0, len(ds))
	for d := range ds {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// toBlobTransfers converts sorted digest strings and a size map into
// a []blobTransfer slice preserving the sort order.
func toBlobTransfers(digestsSorted []string, sizes map[string]int64) []blobTransfer {
	out := make([]blobTransfer, 0, len(digestsSorted))
	for _, d := range digestsSorted {
		out = append(out, blobTransfer{
			Digest: digest.Digest(d),
			Size:   sizes[d],
		})
	}
	return out
}
