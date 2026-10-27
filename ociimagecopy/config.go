package ociimagecopy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/caarlos0/env/v11"
)

const (
	// ENV_OCI_IMAGE_COPY_CONF names the config-file-path override read by configPath.
	//nolint:revive,staticcheck // identifier mirrors the env var name
	ENV_OCI_IMAGE_COPY_CONF = "OCI_IMAGE_COPY_CONF"

	// EnvPrefix is applied to every tag-declared variable via envOptions,
	// yielding OCI_IMAGE_COPY_CACHE_DIR, OCI_IMAGE_COPY_S3_ENDPOINT, etc.
	EnvPrefix = "OCI_IMAGE_COPY_"

	defaultConfigDir = "oci-image-copy"
)

// Config is the materialized configuration after every layer
// (defaults < file < env < flags) is applied.
type Config struct {
	// CacheDir holds downloaded blobs; interrupted pulls resume from it.
	CacheDir string `json:"cache_dir" yaml:"cache_dir"`
	// Registries are mirrors tried, in order, for images missing from a source store.
	Registries []string `json:"registries" yaml:"registries"`
	// CompressionLevel is the zstd level for layers compressed anew:
	// fastest, default, better or best.
	CompressionLevel string   `json:"compression_level" yaml:"compression_level"`
	Concurrency      int      `json:"concurrency" yaml:"concurrency"`
	S3               S3Config `json:"s3" yaml:"s3"`
}

// S3Config adjusts the AWS SDK's default resolution for S3-compatible servers.
// Credentials always come from the SDK's default chain.
type S3Config struct {
	// Endpoint is empty for AWS's own endpoint resolution.
	Endpoint     string `json:"endpoint" yaml:"endpoint"`
	Region       string `json:"region" yaml:"region"`
	UsePathStyle bool   `json:"use_path_style" yaml:"use_path_style"`
}

func DefaultConfig() Config {
	var cacheDir string
	// An unresolvable cache dir is left empty; New reports it only when a copy needs the cache.
	if dir, err := os.UserCacheDir(); err == nil {
		cacheDir = filepath.Join(dir, defaultConfigDir)
	}
	return Config{
		CacheDir:         cacheDir,
		Registries:       nil,
		CompressionLevel: "best",
		Concurrency:      4,
		S3:               S3Config{},
	}
}

//nolint:lll // triple json/yaml/env tags; one field per line, never wrap tags
type PartialConfig struct {
	CacheDir         *string         `json:"cache_dir,omitzero" yaml:"cache_dir,omitempty" env:"CACHE_DIR"`
	Registries       []string        `json:"registries,omitzero" yaml:"registries,omitempty" env:"REGISTRIES"`
	CompressionLevel *string         `json:"compression_level,omitzero" yaml:"compression_level,omitempty" env:"COMPRESSION_LEVEL"`
	Concurrency      *int            `json:"concurrency,omitzero" yaml:"concurrency,omitempty" env:"CONCURRENCY"`
	S3               PartialS3Config `json:"s3,omitzero" yaml:"s3,omitempty" envPrefix:"S3_"`
}

//nolint:lll // triple json/yaml/env tags; one field per line, never wrap tags
type PartialS3Config struct {
	Endpoint     *string `json:"endpoint,omitzero" yaml:"endpoint,omitempty" env:"ENDPOINT"`
	Region       *string `json:"region,omitzero" yaml:"region,omitempty" env:"REGION"`
	UsePathStyle *bool   `json:"use_path_style,omitzero" yaml:"use_path_style,omitempty" env:"USE_PATH_STYLE"`
}

func (p PartialConfig) Apply(base Config) Config {
	if p.CacheDir != nil {
		base.CacheDir = *p.CacheDir
	}
	if p.Registries != nil {
		base.Registries = p.Registries
	}
	if p.CompressionLevel != nil {
		base.CompressionLevel = *p.CompressionLevel
	}
	if p.Concurrency != nil {
		base.Concurrency = *p.Concurrency
	}
	base.S3 = p.S3.Apply(base.S3)
	return base
}

func (p PartialS3Config) Apply(base S3Config) S3Config {
	if p.Endpoint != nil {
		base.Endpoint = *p.Endpoint
	}
	if p.Region != nil {
		base.Region = *p.Region
	}
	if p.UsePathStyle != nil {
		base.UsePathStyle = *p.UsePathStyle
	}
	return base
}

var envOptions = env.Options{Prefix: EnvPrefix}

// LoadConfig assembles defaults < config file < environment through Apply.
// flagPath is the --config value ("" when the flag is unset).
func LoadConfig(flagPath string) (Config, error) {
	cfg := DefaultConfig()

	path, err := configPath(flagPath)
	if err != nil {
		return cfg, err
	}
	filePartial, err := unmarshalConfigFile(path)
	if err != nil {
		return cfg, err
	}
	cfg = filePartial.Apply(cfg)

	var envPartial PartialConfig
	if err := env.ParseWithOptions(&envPartial, envOptions); err != nil {
		return cfg, err
	}
	cfg = envPartial.Apply(cfg)

	return cfg, nil
}

// unmarshalConfigFile decodes into a fresh zero PartialConfig, so it never hits the
// encoding/json merge edge cases of decoding into a populated struct.
func unmarshalConfigFile(path string) (PartialConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PartialConfig{}, nil
		}
		return PartialConfig{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var p PartialConfig
	if err := json.Unmarshal(b, &p); err != nil {
		return PartialConfig{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	return p, nil
}

func configPath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if p, ok := os.LookupEnv(ENV_OCI_IMAGE_COPY_CONF); ok {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, defaultConfigDir, "config.json"), nil
}
