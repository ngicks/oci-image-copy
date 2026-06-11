package imagecopy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
)

// PullArgs configures one [Local.Pull] invocation. Mirror of [PushArgs]
// for the pull direction; flag wiring lives in
// `cmd/oci-image-copy/commands/pull.go`.
type PullArgs struct {
	Images []string

	DryRun bool

	// AssumeLocalHas is the pull-direction equivalent of
	// [PushArgs.AssumeRemoteHas]: a literal digest set short-circuiting
	// local enumeration.
	AssumeLocalHas []string

	// AssumeLocalHasSet is the higher-level form of AssumeLocalHas
	// (already parsed to a digest set). When non-nil it takes
	// precedence over [PullArgs.AssumeLocalHas].
	AssumeLocalHasSet map[string]struct{}

	KeepGoing bool
}

// PullImageReport is the per-image summary line for pulls.
type PullImageReport struct {
	Ref          imageref.ImageRef
	Fetched      int   // blobs actually transferred
	Reused       int   // blobs already present locally
	BytesFetched int64 // sum of expected sizes of transferred blobs
	DryRun       bool
	Err          error
}

// PullResult is the aggregate of per-image pull reports.
type PullResult struct {
	Reports     []PullImageReport
	FailedCount int
}

// Pull orchestrates the pull direction (peer → local).
//
// Pull assumes ref already exists in the peer's OCI mirror. The peer
// is treated as a passive content-addressable store; the orchestrator
// no longer triggers a peer-side `skopeo copy` to materialize the
// image first. Callers wanting to pull from a peer's live storage
// (containers-storage / docker-daemon) need to dump it into the
// peer's mirror separately before calling Pull.
func (l *Local) Pull(ctx context.Context, args PullArgs, peer Remote) (PullResult, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	if err := validatePull(args, l, peer); err != nil {
		return PullResult{}, err
	}
	if err := l.Validate(ctx); err != nil {
		return PullResult{}, err
	}

	localHas, err := resolveLocalHas(ctx, args, l)
	if err != nil {
		return PullResult{}, fmt.Errorf("pull: enumerate local: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "pull.local-has",
		slog.Int("blobs", len(localHas)),
		slog.Bool("from-flag", args.AssumeLocalHasSet != nil || len(args.AssumeLocalHas) > 0),
	)

	var result PullResult
	for _, raw := range args.Images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, err := imageref.Parse(raw)
		if err != nil {
			rep := PullImageReport{
				Ref:    imageref.ImageRef{Original: raw},
				DryRun: args.DryRun,
				Err:    err,
			}
			result.Reports = append(result.Reports, rep)
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("pull %q: %w", raw, err)
			}
			continue
		}
		rep := pullOne(ctx, args, l, peer, localHas, ref)
		result.Reports = append(result.Reports, rep)
		if rep.Err != nil {
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("pull %s: %w", ref.String(), rep.Err)
			}
		}
	}
	return result, nil
}

func validatePull(args PullArgs, local *Local, peer Remote) error {
	if len(args.Images) == 0 {
		return errors.New("pull: no images")
	}
	if local.transport == "" {
		return errors.New("pull: local transport unset")
	}
	if local.baseDir == "" {
		return errors.New("pull: local base dir unset")
	}
	_ = peer
	return nil
}

func resolveLocalHas(
	ctx context.Context,
	args PullArgs,
	local *Local,
) (map[string]struct{}, error) {
	if args.AssumeLocalHasSet != nil {
		return args.AssumeLocalHasSet, nil
	}
	if len(args.AssumeLocalHas) > 0 {
		ds := make(map[string]struct{}, len(args.AssumeLocalHas))
		for _, d := range args.AssumeLocalHas {
			ds[d] = struct{}{}
		}
		return ds, nil
	}
	return local.List(ctx)
}

func pullOne(
	ctx context.Context,
	args PullArgs,
	local *Local,
	peer Remote,
	localHas map[string]struct{},
	ref imageref.ImageRef,
) PullImageReport {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	rep := PullImageReport{Ref: ref, DryRun: args.DryRun}

	mDesc, man, err := ocidir.ReadManifest(ctx, peer.Dir().Image(ref))
	if err != nil {
		rep.Err = fmt.Errorf("read peer manifest: %w", err)
		return rep
	}

	descs := ocidir.AllDescriptors(mDesc, man)
	all := descriptorDigestSet(descs)
	sizes := descriptorSizes(descs)
	pinned := map[string]struct{}{
		string(mDesc.Digest):      {},
		string(man.Config.Digest): {},
	}
	toFetch := mapKeyDiff(all, localHas, pinned)

	for d := range all {
		if _, fetch := toFetch[d]; !fetch {
			rep.Reused++
		}
	}

	digestsSorted := sortedDigests(toFetch)

	if args.DryRun {
		var bytesFetched int64
		for _, d := range digestsSorted {
			bytesFetched += sizes[d]
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "pull.dry-run.plan",
			slog.String("ref", ref.String()),
			slog.Int("blobs", len(digestsSorted)),
			slog.Int64("bytes", bytesFetched),
		)
		rep.Fetched = len(digestsSorted)
		rep.BytesFetched = bytesFetched
		logger.LogAttrs(ctx, slog.LevelInfo, "pull.dry-run.would-load",
			slog.String("ref", ref.String()),
		)
		return rep
	}

	// 1. Mirror tag-dir metadata files from peer to local
	if err := mirrorTagFilesFromPeer(ctx, peer.Dir(), ref, local.Dir()); err != nil {
		rep.Err = fmt.Errorf("tag-dir sync: %w", err)
		return rep
	}

	// 2. Stream missing blobs from peer to local via fsutil Pull primitives
	blobs := toBlobTransfers(digestsSorted, sizes)
	res, err := pullBlobs(ctx, blobs, peer.Dir(), local.fs, local.Dir(), DefaultLocalParallelism)
	if err != nil {
		rep.Err = fmt.Errorf("put blobs: %w", err)
		return rep
	}
	rep.Fetched = res.Sent
	// Blobs in toFetch that were already present locally (e.g. the
	// always-pinned manifest/config on a repeat pull) count as reused too.
	rep.Reused += res.Reused
	rep.BytesFetched = res.BytesSent

	// 3. Load image into local live storage
	if err := local.LoadImage(ctx, ref); err != nil {
		rep.Err = fmt.Errorf("local load: %w", err)
		return rep
	}
	return rep
}

// mirrorTagFilesFromPeer reads index.json + oci-layout from peer's
// tag dir and writes them to local's tag dir via [OciDirs.PutTagFile].
// Both files are small so reading via [ocidir.DirV1.Blob]-like access
// isn't appropriate; we read the JSON via the typed accessors and
// re-marshal.
func mirrorTagFilesFromPeer(
	ctx context.Context,
	src OciDirs,
	ref imageref.ImageRef,
	dst OciDirs,
) error {
	srcDir := src.Image(ref)
	idx, err := srcDir.Index()
	if err != nil {
		return fmt.Errorf("read peer index.json: %w", err)
	}
	layout, err := srcDir.ImageLayout()
	if err != nil {
		return fmt.Errorf("read peer oci-layout: %w", err)
	}
	idxBytes, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index.json: %w", err)
	}
	layoutBytes, err := json.Marshal(layout)
	if err != nil {
		return fmt.Errorf("marshal oci-layout: %w", err)
	}
	if err := dst.PutTagFile(ctx, ref, "oci-layout", layoutBytes); err != nil {
		return fmt.Errorf("put oci-layout: %w", err)
	}
	if err := dst.PutTagFile(ctx, ref, "index.json", idxBytes); err != nil {
		return fmt.Errorf("put index.json: %w", err)
	}
	return nil
}

// SummaryLine returns the human-readable per-image summary string.
func (r PullImageReport) SummaryLine() string {
	if r.Err != nil {
		return fmt.Sprintf("%s ERROR: %v", r.Ref.String(), r.Err)
	}
	prefix := ""
	if r.DryRun {
		prefix = "DRY-RUN would: "
	}
	return fmt.Sprintf("%s%s pulled (new: %d, reused: %d, bytes: %d)",
		prefix, r.Ref.String(), r.Fetched, r.Reused, r.BytesFetched)
}
