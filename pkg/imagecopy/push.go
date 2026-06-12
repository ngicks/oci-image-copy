package imagecopy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// PushArgs configures one [Local.Push] invocation. Flags surfaced via
// the CLI (`cmd/oci-image-copy/commands/push.go`) map 1:1 to fields
// on this struct; keep the cobra side a translation layer only.
type PushArgs struct {
	// Images is the list of refs to push (e.g. "ghcr.io/a/b:c").
	Images []string

	// DryRun replaces all mutating operations (local dump, network
	// transfer, peer load) with read-only equivalents and emits a plan
	// instead of state changes.
	DryRun bool

	// AssumeRemoteHas is a literal digest set ("sha256:..." each) that
	// short-circuits the remote-side enumeration step. Useful when the
	// caller already knows the peer's blob inventory.
	AssumeRemoteHas []string

	// AssumeRemoteHasSet is the higher-level form of AssumeRemoteHas
	// (already parsed to a digest set). When non-nil it takes
	// precedence over [PushArgs.AssumeRemoteHas].
	AssumeRemoteHasSet map[string]struct{}

	// KeepGoing makes per-image errors non-fatal: the run accumulates
	// failures and exits non-zero with a final failure count, rather
	// than short-circuiting on the first error.
	KeepGoing bool
}

// SkopeoLike abstracts [*skopeo.Skopeo] so tests can substitute a fake.
// The methods are the three we drive in push/pull orchestration.
type SkopeoLike interface {
	Version(ctx context.Context) (string, error)
	Inspect(
		ctx context.Context,
		src skopeo.TransportRef,
		raw bool,
		sharedBlobDir string,
		extraArgs ...string,
	) ([]byte, error)
	Copy(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error
}

// PushImageReport is the per-image summary line surfaced in the CLI
// output. Errors land in Err; on success Err is nil.
type PushImageReport struct {
	Ref       imageref.ImageRef
	Sent      int   // blobs actually transferred
	Reused    int   // blobs the peer already had (skipped)
	BytesSent int64 // sum of expected sizes of transferred blobs
	DryRun    bool
	Err       error
}

// PushResult is the aggregate of per-image reports.
type PushResult struct {
	Reports     []PushImageReport
	FailedCount int
}

// Push orchestrates the push direction (local → peer) for every ref in
// args.Images. Honors --dry-run (no mutation anywhere), --keep-going
// (continue on per-image error), and --assume-remote-has (skip
// enumeration of the peer).
func (l *Local) Push(ctx context.Context, args PushArgs, peer Remote) (PushResult, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	if err := validatePush(args, l, peer); err != nil {
		return PushResult{}, err
	}
	if err := l.Validate(ctx); err != nil {
		return PushResult{}, err
	}

	// Parse refs best-effort for RefPrimer.
	var parsedRefs []imageref.ImageRef
	for _, raw := range args.Images {
		if ref, err := imageref.Parse(raw); err == nil {
			parsedRefs = append(parsedRefs, ref)
		}
	}

	remoteHas, err := resolveRemoteHas(ctx, args, peer, parsedRefs)
	if err != nil {
		return PushResult{}, fmt.Errorf("push: enumerate remote: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "push.remote-has",
		slog.Int("blobs", len(remoteHas)),
		slog.Bool("from-flag", args.AssumeRemoteHasSet != nil || len(args.AssumeRemoteHas) > 0),
	)

	var result PushResult
	for _, raw := range args.Images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, err := imageref.Parse(raw)
		if err != nil {
			rep := PushImageReport{
				Ref:    imageref.ImageRef{Original: raw},
				DryRun: args.DryRun,
				Err:    err,
			}
			result.Reports = append(result.Reports, rep)
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("push %q: %w", raw, err)
			}
			continue
		}

		rep := pushOne(ctx, args, l, peer, remoteHas, ref)
		result.Reports = append(result.Reports, rep)
		if rep.Err != nil {
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("push %s: %w", ref.String(), rep.Err)
			}
		}
	}
	return result, nil
}

// validatePush returns an error for missing required-by-transport fields.
func validatePush(args PushArgs, local *Local, peer Remote) error {
	if len(args.Images) == 0 {
		return errors.New("push: no images")
	}
	if local.transport == "" {
		return errors.New("push: local transport unset")
	}
	if local.baseDir == "" {
		return errors.New("push: local base dir unset")
	}
	if peer.ReadOnly() {
		return errors.New("push: peer is read-only")
	}
	return nil
}

// resolveRemoteHas builds the peer-has set, honoring the assume-remote-has
// shortcut.
//
// refs is the list of parsed image refs being pushed, used to prime the
// remote's meta cache via [RefPrimer] when available. This enables accurate
// dry-run plans and blob-level deduplication for file-server remotes.
func resolveRemoteHas(
	ctx context.Context,
	args PushArgs,
	peer Remote,
	refs []imageref.ImageRef,
) (map[string]struct{}, error) {
	if args.AssumeRemoteHasSet != nil {
		return args.AssumeRemoteHasSet, nil
	}
	if len(args.AssumeRemoteHas) > 0 {
		ds := make(map[string]struct{}, len(args.AssumeRemoteHas))
		for _, d := range args.AssumeRemoteHas {
			ds[d] = struct{}{}
		}
		return ds, nil
	}

	// Prime the remote's meta cache if the underlying OciDirs supports it.
	// This loads chunkSize info and blob descriptors from existing image metas,
	// enabling accurate ListBlobs output and correct BlobSource chunk sizes.
	if primer, ok := peer.Dir().(RefPrimer); ok && len(refs) > 0 {
		// Best-effort: ignore priming errors.
		_ = primer.PrimeRefs(ctx, refs)
	}

	out := make(map[string]struct{})
	for d, err := range peer.ListBlobs(ctx) {
		if err != nil {
			return nil, err
		}
		out[string(d)] = struct{}{}
	}
	return out, nil
}

func pushOne(
	ctx context.Context,
	args PushArgs,
	local *Local,
	peer Remote,
	remoteHas map[string]struct{},
	ref imageref.ImageRef,
) PushImageReport {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	rep := PushImageReport{Ref: ref, DryRun: args.DryRun}

	mDesc, man, err := dumpAndDeriveClosurePush(ctx, args, local, ref)
	if err != nil {
		rep.Err = fmt.Errorf("dump: %w", err)
		return rep
	}

	descs := ocidir.AllDescriptors(mDesc, man)
	all := descriptorDigestSet(descs)
	sizes := descriptorSizes(descs)
	pinned := map[string]struct{}{
		string(mDesc.Digest):      {},
		string(man.Config.Digest): {},
	}
	toSend := mapKeyDiff(all, remoteHas, pinned)

	for d := range all {
		if _, send := toSend[d]; !send {
			rep.Reused++
		}
	}

	digestsSorted := sortedDigests(toSend)

	if args.DryRun {
		// In dry-run mode, use BlobProber (if available) to probe blobs that
		// are not yet in remoteHas. This enables accurate Sent/Reused counts
		// without any mutating operations.
		if prober, ok := peer.Dir().(BlobProber); ok {
			for _, d := range digestsSorted {
				dgst := digest.Digest(d)
				complete, err := prober.ProbeBlob(ctx, dgst, sizes[d])
				if err == nil && complete {
					// Blob is already fully present; move from toSend to reused.
					delete(toSend, d)
					rep.Reused++
				}
			}
			// Recompute digestsSorted after probing.
			digestsSorted = sortedDigests(toSend)
		}

		var bytesSent int64
		for _, d := range digestsSorted {
			bytesSent += sizes[d]
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "push.dry-run.plan",
			slog.String("ref", ref.String()),
			slog.Int("blobs", len(digestsSorted)),
			slog.Int64("bytes", bytesSent),
		)
		rep.Sent = len(digestsSorted)
		rep.BytesSent = bytesSent
		logger.LogAttrs(ctx, slog.LevelInfo, "push.dry-run.would-load",
			slog.String("ref", ref.String()),
		)
		return rep
	}

	// 1. Stream missing blobs to the peer via fsutil Push primitives.
	// Blobs are transferred BEFORE tag-dir metadata (commit-last semantics):
	// the peer's tag dir references only blobs that are already fully present,
	// which improves crash consistency for all transport backends.
	blobs := toBlobTransfers(digestsSorted, sizes)
	res, err := pushBlobs(ctx, blobs, local.fs, peer.Dir(), DefaultRemoteParallelism)
	if err != nil {
		rep.Err = fmt.Errorf("put blobs: %w", err)
		return rep
	}
	rep.Sent = res.Sent
	// Blobs in toSend that the peer turned out to already hold (e.g. the
	// always-pinned manifest/config on a repeat push) count as reused too.
	rep.Reused += res.Reused
	rep.BytesSent = res.BytesSent

	// 2. Mirror tag-dir metadata files to the peer (commit step).
	// Written after all blobs so the tag dir only references present blobs.
	if err := mirrorTagFiles(ctx, local.fs, ref, peer.Dir()); err != nil {
		rep.Err = fmt.Errorf("tag-dir sync: %w", err)
		return rep
	}

	// 3. Load image on peer (mirror → live storage)
	if err := peer.LoadImage(ctx, ref); err != nil {
		rep.Err = fmt.Errorf("remote load: %w", err)
		return rep
	}
	return rep
}

// dumpAndDeriveClosurePush runs `skopeo copy ... oci:<tagDir>` and returns the
// manifest descriptor + parsed manifest body. In dry-run mode the copy targets
// a temporary store so the transfer plan observes the same destination
// compression as a real push without mutating the configured local store.
func dumpAndDeriveClosurePush(
	ctx context.Context,
	args PushArgs,
	local *Local,
	ref imageref.ImageRef,
) (v1.Descriptor, v1.Manifest, error) {
	src := localTransportRef(local.transport, local.ociPath, ref)

	baseDir := local.baseDir
	dstFS := local.fs
	dstDirs := local.Dir()
	if args.DryRun {
		tmp, err := os.MkdirTemp("", "oci-image-copy-dry-run-*")
		if err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("dry-run temp store: %w", err)
		}
		defer os.RemoveAll(tmp)
		if err := NewStore(tmp).EnsureLayout(ctx); err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("dry-run temp layout: %w", err)
		}
		fs, err := NewOsFs(tmp)
		if err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("dry-run temp fs: %w", err)
		}
		baseDir = tmp
		dstFS = fs
		dstDirs = NewFsOciDirs(fs, DefaultLocalParallelism)
	}

	store := NewStore(baseDir)
	tagDirAbs, err := store.DumpDir(ref)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, err
	}
	tagDirRel, err := RelDumpDir(ref)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, err
	}
	if err := dstFS.MkdirAll(tagDirRel, 0o755); err != nil {
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("mkdir %s: %w", tagDirRel, err)
	}
	if err := local.skopeoCli.Copy(
		ctx,
		src,
		skopeo.TransportRef{
			Transport: skopeo.TransportOci,
			Arg1:      tagDirAbs,
			Arg2:      ref.String(),
		},
		store.ShareDir(),
	); err != nil {
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("skopeo copy: %w", err)
	}
	return ocidir.ReadManifest(ctx, dstDirs.Image(ref))
}

// mirrorTagFiles ships oci-layout + index.json from srcFS's per-ref
// tag dir to dst (the destination [OciDirs]).
func mirrorTagFiles(
	ctx context.Context,
	srcFS vroot.Fs[vroot.File],
	ref imageref.ImageRef,
	dst OciDirs,
) error {
	rel, err := RelDumpDir(ref)
	if err != nil {
		return err
	}
	for _, name := range []string{"oci-layout", "index.json"} {
		data, err := vroot.ReadFile(srcFS, path.Join(rel, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := dst.PutTagFile(ctx, ref, name, data); err != nil {
			return fmt.Errorf("put %s: %w", name, err)
		}
	}
	return nil
}

// SummaryLine returns the human-readable per-image summary string.
func (r PushImageReport) SummaryLine() string {
	if r.Err != nil {
		return fmt.Sprintf("%s ERROR: %v", r.Ref.String(), r.Err)
	}
	prefix := ""
	if r.DryRun {
		prefix = "DRY-RUN would: "
	}
	return fmt.Sprintf("%s%s pushed (new: %d, reused: %d, bytes: %d)",
		prefix, r.Ref.String(), r.Sent, r.Reused, r.BytesSent)
}
