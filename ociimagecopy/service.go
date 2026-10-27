package ociimagecopy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"go.podman.io/storage"

	"github.com/ngicks/oci-image-copy/blobcache"
	"github.com/ngicks/oci-image-copy/cstorage"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/ocidir/httpdir"
	"github.com/ngicks/oci-image-copy/ocidir/s3dir"
	"github.com/ngicks/oci-image-copy/registry"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
)

var ErrInvalidConfig = errors.New("ociimagecopy: invalid configuration")

// Options carries what a programmatic caller may inject instead of the host defaults.
type Options struct {
	// AdjustStorage changes podman's store options before containers-storage opens.
	AdjustStorage func(*storage.StoreOptions)
	// Fallback resolves entries a source store lacks; nil means the registries, through
	// the image's own repository and then Config.Registries.
	Fallback transfer.Source
	// S3Client serves s3 locations; nil builds one from the AWS default chain and Config.S3.
	S3Client s3dir.StoreClient
}

// Service opens the blob cache, containers-storage and the S3 client on first use and keeps them
// until Close.
type Service struct {
	cfg   Config
	opt   Options
	level zstd.EncoderLevel

	mu    sync.Mutex
	cache *blobcache.Cache
	cs    *cstorage.Storage
	s3    s3dir.StoreClient
}

func New(cfg Config, opt *Options) (*Service, error) {
	ok, level := zstd.EncoderLevelFromString(cfg.CompressionLevel)
	if !ok {
		return nil, fmt.Errorf(
			"%w: compression_level %q: want fastest, default, better or best",
			ErrInvalidConfig,
			cfg.CompressionLevel,
		)
	}
	if cfg.Concurrency < 1 {
		return nil, fmt.Errorf(
			"%w: concurrency %d: want 1 or more",
			ErrInvalidConfig,
			cfg.Concurrency,
		)
	}
	s := &Service{cfg: cfg, level: level}
	if opt != nil {
		s.opt = *opt
	}
	return s, nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if s.cs != nil {
		errs = append(errs, s.cs.Close())
		s.cs = nil
	}
	if s.cache != nil {
		errs = append(errs, s.cache.Close())
		s.cache = nil
	}
	return errors.Join(errs...)
}

func (s *Service) blobCache() (*blobcache.Cache, error) {
	if s.cache != nil {
		return s.cache, nil
	}
	if s.cfg.CacheDir == "" {
		return nil, fmt.Errorf(
			"%w: cache_dir is empty and no user cache directory is known",
			ErrInvalidConfig,
		)
	}
	c, err := blobcache.New(filepath.Join(s.cfg.CacheDir, "blobcache"))
	if err != nil {
		return nil, fmt.Errorf("ociimagecopy: opening the blob cache: %w", err)
	}
	s.cache = c
	return c, nil
}

func (s *Service) containersStorage() (*cstorage.Storage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cs != nil {
		return s.cs, nil
	}
	cache, err := s.blobCache()
	if err != nil {
		return nil, err
	}
	fallback := s.opt.Fallback
	if fallback == nil {
		fallback = registry.New(cache, &registry.Options{Registries: s.cfg.Registries})
	}
	cs, err := cstorage.Open(s.opt.AdjustStorage, cache, &cstorage.Options{
		Fallback:         fallback,
		CompressionLevel: s.level,
	})
	if err != nil {
		return nil, fmt.Errorf("ociimagecopy: opening containers-storage: %w", err)
	}
	s.cs = cs
	return cs, nil
}

func (s *Service) s3Client(ctx context.Context) (s3dir.StoreClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opt.S3Client != nil {
		return s.opt.S3Client, nil
	}
	if s.s3 != nil {
		return s.s3, nil
	}
	var opts []func(*awsconfig.LoadOptions) error
	if s.cfg.S3.Region != "" {
		opts = append(opts, awsconfig.WithRegion(s.cfg.S3.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("ociimagecopy: loading AWS configuration: %w", err)
	}
	s.s3 = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if s.cfg.S3.Endpoint != "" {
			o.BaseEndpoint = aws.String(s.cfg.S3.Endpoint)
		}
		o.UsePathStyle = s.cfg.S3.UsePathStyle
	})
	return s.s3, nil
}

// openStore opens the store at loc; create makes a missing filesystem store directory.
func (s *Service) openStore(
	ctx context.Context,
	loc Location,
	create bool,
) (*store.Store, func() error, error) {
	noop := func() error { return nil }
	switch loc.URL.Scheme {
	case "file":
		dir := filepath.FromSlash(loc.URL.Path)
		if create {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, fmt.Errorf("ociimagecopy: creating %s: %w", loc, err)
			}
		}
		root, err := osfs.NewRoot(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("ociimagecopy: opening %s: %w", loc, err)
		}
		return store.New(fsdir.NewStore(root)), root.Close, nil
	case "s3":
		client, err := s.s3Client(ctx)
		if err != nil {
			return nil, nil, err
		}
		bucket, prefix := s3Location(loc.URL)
		return store.New(s3dir.NewStore(client, s3dir.FlatStore(bucket, prefix))), noop, nil
	default:
		return store.New(httpdir.NewStore(httpdir.BaseURLStore(loc.URL.String()), nil)), noop, nil
	}
}
