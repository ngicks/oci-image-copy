package ociimagecopy

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadConfig_layers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if def := DefaultConfig(); cfg.Concurrency != def.Concurrency ||
		cfg.CompressionLevel != "best" {
		t.Fatalf("no file and no env: got %+v, want defaults", cfg)
	}

	file := filepath.Join(dir, "config.json")
	err = os.WriteFile(
		file,
		[]byte(
			`{"concurrency":2,"registries":["a.example"],"s3":{"region":"r1","use_path_style":true}}`,
		),
		0o644,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCI_IMAGE_COPY_CONCURRENCY", "8")
	t.Setenv("OCI_IMAGE_COPY_S3_ENDPOINT", "http://s3.example")

	cfg, err = LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != 8 {
		t.Errorf("env overrides file: concurrency = %d, want 8", cfg.Concurrency)
	}
	if !slices.Equal(cfg.Registries, []string{"a.example"}) {
		t.Errorf("file overrides defaults: registries = %v", cfg.Registries)
	}
	want := S3Config{Endpoint: "http://s3.example", Region: "r1", UsePathStyle: true}
	if cfg.S3 != want {
		t.Errorf("s3 deep-merges file and env: got %+v, want %+v", cfg.S3, want)
	}
	if cfg.CompressionLevel != "best" {
		t.Errorf("unset keys keep defaults: compression_level = %q", cfg.CompressionLevel)
	}
}

func TestLoadConfig_badEnv(t *testing.T) {
	t.Setenv("OCI_IMAGE_COPY_CONF", filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv("OCI_IMAGE_COPY_CONCURRENCY", "many")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("unparsable env value: want error")
	}
}
