package ociimagecopy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Local != "containers-storage:" {
		t.Errorf("DefaultConfig().Local = %q, want %q", cfg.Local, "containers-storage:")
	}
	if cfg.LocalDumpDir != "" {
		t.Errorf("DefaultConfig().LocalDumpDir = %q, want empty", cfg.LocalDumpDir)
	}
	fs := cfg.FileServer
	if fs.Auth != "" || fs.Headers != nil || fs.ChunkSize != "" || fs.NamingPrefix != "" {
		t.Errorf("DefaultConfig().FileServer = %+v, want zero", cfg.FileServer)
	}
	// The zstd default is filled here (the single source of truth), not deferred.
	c := cfg.Compression
	if c.Format != DefaultCompressionFormat || c.Level != DefaultCompressionLevel || !c.Force {
		t.Errorf(
			"DefaultConfig().Compression = %+v, want {%s %d true}",
			c, DefaultCompressionFormat, DefaultCompressionLevel,
		)
	}
}

func TestPartialConfigApply_ScalarOverwrite(t *testing.T) {
	base := DefaultConfig()
	p := PartialConfig{
		Local:        new("docker://"),
		LocalDumpDir: new("/tmp/dump"),
	}
	got := p.Apply(base)
	if got.Local != "docker://" {
		t.Errorf("Local = %q, want %q", got.Local, "docker://")
	}
	if got.LocalDumpDir != "/tmp/dump" {
		t.Errorf("LocalDumpDir = %q, want %q", got.LocalDumpDir, "/tmp/dump")
	}
}

func TestPartialConfigApply_ExplicitZeroOverwrite(t *testing.T) {
	base := DefaultConfig() // Local == "containers-storage:"
	p := PartialConfig{
		Local: new(""), // explicit zero must win over the non-empty default
	}
	got := p.Apply(base)
	if got.Local != "" {
		t.Errorf("explicit empty Local should overwrite default; got %q", got.Local)
	}
}

func TestPartialConfigApply_NilLeavesBase(t *testing.T) {
	base := DefaultConfig()
	var p PartialConfig // all nil
	got := p.Apply(base)
	if got.Local != base.Local || got.LocalDumpDir != base.LocalDumpDir {
		t.Errorf("zero PartialConfig must leave base unchanged; got %+v", got)
	}
}

func TestPartialFileServerConfigApply_HeadersSliceOverwrite(t *testing.T) {
	base := FileServerConfig{Headers: []string{"X-Old: 1", "X-Keep: 2"}}

	// non-nil incoming slice overwrites wholesale.
	p := PartialFileServerConfig{Headers: []string{"X-New: 9"}}
	got := p.Apply(base)
	if len(got.Headers) != 1 || got.Headers[0] != "X-New: 9" {
		t.Errorf("Headers should overwrite wholesale; got %v", got.Headers)
	}

	// nil incoming slice leaves the base.
	var pNil PartialFileServerConfig
	gotNil := pNil.Apply(base)
	if len(gotNil.Headers) != 2 {
		t.Errorf("nil Headers must leave base; got %v", gotNil.Headers)
	}

	// explicit empty slice replaces with empty.
	pEmpty := PartialFileServerConfig{Headers: []string{}}
	gotEmpty := pEmpty.Apply(base)
	if gotEmpty.Headers == nil || len(gotEmpty.Headers) != 0 {
		t.Errorf("explicit empty Headers must replace with empty; got %v", gotEmpty.Headers)
	}
}

func TestPartialConfigApply_NestedDeepMergePreservesSiblings(t *testing.T) {
	base := DefaultConfig()
	base.FileServer = FileServerConfig{
		Auth:         "Bearer keep",
		Headers:      []string{"X-Keep: 1"},
		ChunkSize:    "4MiB",
		NamingPrefix: "keep/",
	}

	// Only set NamingPrefix; every sibling field must survive.
	p := PartialConfig{
		FileServer: PartialFileServerConfig{NamingPrefix: new("new/")},
	}
	got := p.Apply(base)
	if got.FileServer.NamingPrefix != "new/" {
		t.Errorf("NamingPrefix = %q, want %q", got.FileServer.NamingPrefix, "new/")
	}
	if got.FileServer.Auth != "Bearer keep" {
		t.Errorf("Auth sibling not preserved: %q", got.FileServer.Auth)
	}
	if got.FileServer.ChunkSize != "4MiB" {
		t.Errorf("ChunkSize sibling not preserved: %q", got.FileServer.ChunkSize)
	}
	if len(got.FileServer.Headers) != 1 || got.FileServer.Headers[0] != "X-Keep: 1" {
		t.Errorf("Headers sibling not preserved: %v", got.FileServer.Headers)
	}
}

func TestPartialConfigApply_ZeroNestedMergesNothing(t *testing.T) {
	base := DefaultConfig()
	base.FileServer = FileServerConfig{
		Auth:         "Bearer keep",
		ChunkSize:    "4MiB",
		NamingPrefix: "keep/",
	}
	p := PartialConfig{} // FileServer is a zero PartialFileServerConfig
	got := p.Apply(base)
	g, b := got.FileServer, base.FileServer
	if g.Auth != b.Auth || g.ChunkSize != b.ChunkSize || g.NamingPrefix != b.NamingPrefix ||
		len(g.Headers) != len(b.Headers) {
		t.Errorf(
			"zero nested partial must merge nothing; got %+v want %+v",
			got.FileServer,
			base.FileServer,
		)
	}
}

func TestPartialCompressionConfigApply(t *testing.T) {
	base := CompressionConfig{Format: "zstd", Level: 20, Force: true}

	// Scalar overwrite, including an explicit zero (Force=false).
	p := PartialCompressionConfig{
		Format: new("gzip"),
		Level:  new(9),
		Force:  new(false),
	}
	got := p.Apply(base)
	if got.Format != "gzip" || got.Level != 9 || got.Force {
		t.Errorf("Apply overwrite = %+v, want {gzip 9 false}", got)
	}

	// Nil fields leave the base untouched.
	var pNil PartialCompressionConfig
	gotNil := pNil.Apply(base)
	if gotNil != base {
		t.Errorf("zero partial must leave base; got %+v want %+v", gotNil, base)
	}

	// Setting one field preserves its siblings (nested deep-merge semantics).
	pOne := PartialCompressionConfig{Level: new(10)}
	gotOne := pOne.Apply(base)
	if gotOne.Format != "zstd" || gotOne.Level != 10 || !gotOne.Force {
		t.Errorf("partial Level merge = %+v, want {zstd 10 true}", gotOne)
	}
}

func TestLoadConfig_CompressionDefault(t *testing.T) {
	// Isolate from any real on-disk config file: point the path at a
	// non-existent file so only defaults (+ no env) apply.
	t.Setenv(envConfVar, filepath.Join(t.TempDir(), "absent.json"))

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	c := cfg.Compression
	if c.Format != DefaultCompressionFormat || c.Level != DefaultCompressionLevel || !c.Force {
		t.Errorf(
			"default Compression = %+v, want {%s %d true}",
			c, DefaultCompressionFormat, DefaultCompressionLevel,
		)
	}
}

func TestLoadConfig_EnvOverlay_Compression(t *testing.T) {
	t.Setenv(envConfVar, filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv("OCI_IMAGE_COPY_COMPRESSION_FORMAT", "gzip")
	t.Setenv("OCI_IMAGE_COPY_COMPRESSION_LEVEL", "6")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Env overrides Format and Level per-field; the unset Force keeps the
	// filled default (true). This is the per-field merge over DefaultConfig.
	c := cfg.Compression
	if c.Format != "gzip" || c.Level != 6 || !c.Force {
		t.Errorf("env-set Compression = %+v, want {gzip 6 true}", c)
	}
}

func TestLoadConfig_EnvOverlay_Auth(t *testing.T) {
	t.Setenv("OCI_IMAGE_COPY_FILESERVER_AUTH", "Bearer tok")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.FileServer.Auth != "Bearer tok" {
		t.Errorf("FileServer.Auth = %q, want %q", cfg.FileServer.Auth, "Bearer tok")
	}
}

func TestLoadConfig_EnvOverlay_Local(t *testing.T) {
	t.Setenv("OCI_IMAGE_COPY_LOCAL", "docker://")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Local != "docker://" {
		t.Errorf("Local = %q, want %q", cfg.Local, "docker://")
	}
}

func TestFileServerConfig_RedactAuth(t *testing.T) {
	const secret = "Bearer super-secret-token"
	cfg := Config{
		Local: "containers-storage:",
		FileServer: FileServerConfig{
			Auth:         secret,
			NamingPrefix: "p/",
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(b)
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("marshaled config should contain [REDACTED]; got %s", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("marshaled config must NOT contain the secret %q; got %s", secret, out)
	}

	// In-memory field access still returns the real value.
	if cfg.FileServer.Auth != secret {
		t.Errorf("in-memory Auth must stay real; got %q", cfg.FileServer.Auth)
	}
}

func TestFileServerConfig_EmptyAuthNotRedacted(t *testing.T) {
	cfg := FileServerConfig{Auth: ""}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(b)
	if strings.Contains(out, "[REDACTED]") {
		t.Errorf("empty Auth must not be redacted; got %s", out)
	}
	if !strings.Contains(out, `"auth":""`) {
		t.Errorf("empty Auth should marshal as empty string; got %s", out)
	}
}

func TestConfigPath_FlagWins(t *testing.T) {
	// Even with the env var set, an explicit flag path wins.
	t.Setenv(envConfVar, "/from/env.json")
	got, err := configPath("/from/flag.json")
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	if got != "/from/flag.json" {
		t.Errorf("flag path should win; got %q", got)
	}
}

func TestConfigPath_EnvUsedWhenFlagEmpty(t *testing.T) {
	want := filepath.FromSlash("/from/env.json")
	t.Setenv(envConfVar, want)
	got, err := configPath("")
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	if got != want {
		t.Errorf("env path should be used when flag empty; got %q want %q", got, want)
	}
}
