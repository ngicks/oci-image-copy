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

// Config is the materialized configuration the service consumes, after every
// layer (defaults < file < env < flags) is applied. Its fields are value types
// (the merged config is always concrete) and carry both json and yaml tags so the
// `config` subcommand can marshal it and a project can adopt either file format
// without touching fields. The file is NOT decoded into Config — PartialConfig is
// the decode target; Config only ever holds a fully-merged result.
type Config struct {
	Local        string            `json:"local" yaml:"local"`
	LocalDumpDir string            `json:"localDumpDir" yaml:"localDumpDir"`
	Compression  CompressionConfig `json:"compression" yaml:"compression"` // deep-merged
	FileServer   FileServerConfig  `json:"fileServer" yaml:"fileServer"`   // deep-merged
}

// FileServerConfig is a sub-config describing the remote file server. Auth is a
// secret: its real value lives in memory (callers read it directly) but is
// redacted on marshal via MarshalJSON so the `config` subcommand never prints it.
type FileServerConfig struct {
	Auth         string   `json:"auth" yaml:"auth"` // secret; redacted on marshal
	Headers      []string `json:"headers" yaml:"headers"`
	ChunkSize    string   `json:"chunkSize" yaml:"chunkSize"`
	NamingPrefix string   `json:"namingPrefix" yaml:"namingPrefix"`
}

// MarshalJSON marshals FileServerConfig with a non-empty Auth replaced by the
// literal "[REDACTED]" so the secret never leaks into the `config` subcommand
// output. An empty Auth stays empty. The alias type sheds the MarshalJSON method
// to avoid infinite recursion; this affects only marshaling — in-memory field
// access still returns the real value.
func (c FileServerConfig) MarshalJSON() ([]byte, error) {
	type alias FileServerConfig
	redacted := alias(c)
	if redacted.Auth != "" {
		redacted.Auth = "[REDACTED]"
	}
	return json.Marshal(redacted)
}

// DefaultCompressionFormat and DefaultCompressionLevel are the package-wide
// skopeo destination-compression defaults. This is the single source of the
// zstd default: it is baked into [DefaultConfig] (the lowest config layer) and
// flows out from there. Constructors such as [NewSkopeoWithCompression] apply
// whatever config reaches them verbatim and never re-inject a default.
const (
	DefaultCompressionFormat = "zstd"
	DefaultCompressionLevel  = 20
)

// DefaultConfig is the lowest-precedence layer. Initialize maps and sub-configs
// here so later layers deep-merge into a populated base.
func DefaultConfig() Config {
	return Config{
		Local:        "containers-storage:",
		LocalDumpDir: "",
		// The compression default lives here, the single source of truth: zstd,
		// level 20, with forced recompression so already-gzip source layers are
		// rewritten to zstd. A file/env layer overrides any field; note that
		// overriding Format alone (e.g. to gzip) leaves Level at 20, which is
		// out of gzip's 1-9 range — set Level too when changing Format.
		Compression: CompressionConfig{
			Format: DefaultCompressionFormat,
			Level:  DefaultCompressionLevel,
			Force:  true,
		},
		FileServer: FileServerConfig{},
	}
}

// PartialConfig is the exported sparse mirror of Config's serialized shape (the
// same keys): a nil/zero field means "absent, leave the lower layer"; a set field
// is an explicit value, including an explicit zero. It is the decode target for
// the config file (JSON or YAML) AND the struct LoadConfig fills via
// caarlos0/env, so file and env merge through one method, Apply. Exported so other
// code can build or inspect partial overrides.
//
// Carries three tag sets, kept in sync with Config field-for-field: json + yaml
// for the file decode, and env / envPrefix for caarlos0/env. Field kinds:
//   - scalar:        *T (nil = absent; *false / *0 = explicit zero). env:"NAME".
//   - nested struct: a VALUE PartialXxx (not a pointer) with its own Apply, and
//     envPrefix:"NAME_". caarlos0/env recurses into value nested
//     structs but leaves nil pointer-structs unset; a zero value
//     sub-partial still means "nothing set" for the file too.
//   - map / slice:   plain map/slice (nil = absent). env:"NAME"; caarlos0/env
//     parses "k:v,k2:v2" / "a,b,c" natively.
//
// The OCI_IMAGE_COPY_ prefix is applied once via envOptions, so the tags hold only
// the bare names (LOCAL -> OCI_IMAGE_COPY_LOCAL, FILESERVER_ + AUTH ->
// ..._FILESERVER_AUTH).
//
// JSON tags use ",omitzero" (Go 1.24+): when a PartialConfig is marshaled back out
// (e.g. to write a sparse override file or a diff), omitzero drops nil/absent
// fields but preserves an EXPLICIT zero — including a non-nil empty []/{}, which
// the merge rules treat as "present, set to empty". (Tags affect only marshaling,
// never decoding, so this is free on the load path.)
//
// YAML has no omitzero, so the yaml tags use ",omitempty" — which DOES drop a
// non-nil empty []/{} on marshal. Decoding is unaffected (the normal path); only a
// YAML round-trip of a partial loses the explicit-empty signal. Marshal partials
// to JSON when that distinction matters.
//
//nolint:lll // triple json/yaml/env tags; one field per line, never wrap tags
type PartialConfig struct {
	Local        *string                  `json:"local,omitzero" yaml:"local,omitempty" env:"LOCAL"`
	LocalDumpDir *string                  `json:"localDumpDir,omitzero" yaml:"localDumpDir,omitempty" env:"LOCAL_DUMPDIR"`
	Compression  PartialCompressionConfig `json:"compression,omitzero" yaml:"compression,omitempty" envPrefix:"COMPRESSION_"`
	FileServer   PartialFileServerConfig  `json:"fileServer,omitzero" yaml:"fileServer,omitempty" envPrefix:"FILESERVER_"`
}

//nolint:lll // triple json/yaml/env tags; one field per line, never wrap tags
type PartialCompressionConfig struct {
	Format *string `json:"format,omitzero" yaml:"format,omitempty" env:"FORMAT"`
	Level  *int    `json:"level,omitzero" yaml:"level,omitempty" env:"LEVEL"`
	Force  *bool   `json:"force,omitzero" yaml:"force,omitempty" env:"FORCE"`
}

//nolint:lll // triple json/yaml/env tags; one field per line, never wrap tags
type PartialFileServerConfig struct {
	Auth         *string  `json:"auth,omitzero" yaml:"auth,omitempty" env:"AUTH"`
	Headers      []string `json:"headers,omitzero" yaml:"headers,omitempty" env:"HEADERS"`
	ChunkSize    *string  `json:"chunkSize,omitzero" yaml:"chunkSize,omitempty" env:"CHUNK_SIZE"`
	NamingPrefix *string  `json:"namingPrefix,omitzero" yaml:"namingPrefix,omitempty" env:"NAMING_PREFIX"`
}

// Apply overlays p's present fields onto base and returns the merged Config.
// Merge rules by field kind:
//   - scalar:        non-nil pointer overwrites (explicit zero included).
//   - nested struct: deep-merged via the sub-partial's Apply — always called; a
//     zero sub-partial (all fields nil) merges nothing.
//   - slice/array:   non-nil incoming slice overwrites wholesale (nil = leave base).
func (p PartialConfig) Apply(base Config) Config {
	if p.Local != nil {
		base.Local = *p.Local
	}
	if p.LocalDumpDir != nil {
		base.LocalDumpDir = *p.LocalDumpDir
	}
	base.Compression = p.Compression.Apply(base.Compression)
	base.FileServer = p.FileServer.Apply(base.FileServer)
	return base
}

// Apply overlays p's present fields onto base and returns the merged
// CompressionConfig. Each scalar overwrites when non-nil (explicit zero
// included), so a config file or env var can independently set the skopeo
// destination compression Format, Level, and Force.
func (p PartialCompressionConfig) Apply(base CompressionConfig) CompressionConfig {
	if p.Format != nil {
		base.Format = *p.Format
	}
	if p.Level != nil {
		base.Level = *p.Level
	}
	if p.Force != nil {
		base.Force = *p.Force
	}
	return base
}

// Apply overlays p's present fields onto base and returns the merged
// FileServerConfig. Scalars overwrite when non-nil (explicit zero included); the
// Headers slice overwrites wholesale when non-nil (nil leaves the base).
func (p PartialFileServerConfig) Apply(base FileServerConfig) FileServerConfig {
	if p.Auth != nil {
		base.Auth = *p.Auth
	}
	if p.Headers != nil {
		base.Headers = p.Headers
	}
	if p.ChunkSize != nil {
		base.ChunkSize = *p.ChunkSize
	}
	if p.NamingPrefix != nil {
		base.NamingPrefix = *p.NamingPrefix
	}
	return base
}

// envOptions configures caarlos0/env for the env layer in LoadConfig. The
// variable names live in the env: / envPrefix: tags on PartialConfig; the
// OCI_IMAGE_COPY_ prefix is applied here, yielding OCI_IMAGE_COPY_LOCAL,
// OCI_IMAGE_COPY_FILESERVER_AUTH, etc.
var envOptions = env.Options{Prefix: "OCI_IMAGE_COPY_"}

// LoadConfig assembles defaults < config file < environment through Apply. The
// ./cmd layer applies explicitly-set flags on top (flags win). flagPath is the
// --config value ("" when the flag is unset). Rename to config.Load in a sub-package.
//
// The env layer fills a PartialConfig with caarlos0/env: a scalar/slice is set
// (non-nil) only when its variable is present; absent ones stay nil so Apply
// leaves the lower layer untouched. caarlos0/env parses slices ("a,b,c") natively
// and recurses into the value nested sub-config via envPrefix. ParseWithOptions
// errors when a present value fails to parse — a hard error that aborts startup.
// t.Setenv + LoadConfig keeps the layer unit-testable.
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

// unmarshalConfigFile only reads + decodes; it never merges. It decodes into a
// fresh zero PartialConfig (all nil) and returns the zero value when the file
// does not exist. A non-ENOENT read error or a JSON parse error aborts.
//
// Decoding into a zero value — never a defaults-populated struct — sidesteps the
// v1 encoding/json merge edge cases that decoding into a populated struct hits;
// Apply does the merge afterward.
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

// envConfVar names the config-file-path override. It is the one env var read by
// hand (the file path is needed before parsing, and is not a Config field); every
// other variable lives in PartialConfig's env tags. MixedCaps, so no naming-lint
// directive is needed.
const envConfVar = "OCI_IMAGE_COPY_CONF"

// configPath resolves the file path: --config (flagPath), else $envConfVar, else
// os.UserConfigDir()/oci-image-copy/config.json.
func configPath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if p, ok := os.LookupEnv(envConfVar); ok {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "oci-image-copy", "config.json"), nil
}
