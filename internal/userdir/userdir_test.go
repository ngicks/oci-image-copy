package userdir

import (
	"os"
	"runtime"
	"strconv"
	"testing"
)

// The darwin/windows branches resolve from platform-native env vars that do
// not exist on the CI host; only the Unix branches are asserted here.
func skipUnlessUnix(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "windows", "darwin", "ios", "plan9":
		t.Skipf("unix-only assertions; GOOS=%s", runtime.GOOS)
	}
}

func TestData(t *testing.T) {
	skipUnlessUnix(t)

	t.Run("xdg override", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		dir, err := Data()
		if err != nil {
			t.Fatalf("Data() error: %v", err)
		}
		if dir != "/custom/data" {
			t.Errorf("Data() = %q, want %q", dir, "/custom/data")
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/u")
		dir, err := Data()
		if err != nil {
			t.Fatalf("Data() error: %v", err)
		}
		if dir != "/home/u/.local/share" {
			t.Errorf("Data() = %q, want %q", dir, "/home/u/.local/share")
		}
	})

	t.Run("relative xdg rejected", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "relative/path")
		if _, err := Data(); err == nil {
			t.Error("Data() succeeded with relative $XDG_DATA_HOME, want error")
		}
	})

	t.Run("no env", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "")
		if _, err := Data(); err == nil {
			t.Error("Data() succeeded with no env, want error")
		}
	})
}

func TestState(t *testing.T) {
	skipUnlessUnix(t)

	t.Run("xdg override", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/custom/state")
		dir, err := State()
		if err != nil {
			t.Fatalf("State() error: %v", err)
		}
		if dir != "/custom/state" {
			t.Errorf("State() = %q, want %q", dir, "/custom/state")
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "/home/u")
		dir, err := State()
		if err != nil {
			t.Fatalf("State() error: %v", err)
		}
		if dir != "/home/u/.local/state" {
			t.Errorf("State() = %q, want %q", dir, "/home/u/.local/state")
		}
	})

	t.Run("relative xdg rejected", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "relative/path")
		if _, err := State(); err == nil {
			t.Error("State() succeeded with relative $XDG_STATE_HOME, want error")
		}
	})

	t.Run("no env", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "")
		if _, err := State(); err == nil {
			t.Error("State() succeeded with no env, want error")
		}
	})
}

func TestRuntime(t *testing.T) {
	skipUnlessUnix(t)

	t.Run("set", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		dir, err := Runtime()
		if err != nil {
			t.Fatalf("Runtime() error: %v", err)
		}
		if dir != "/run/user/1000" {
			t.Errorf("Runtime() = %q, want %q", dir, "/run/user/1000")
		}
	})

	t.Run("unset falls back to temp", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", "/tmp-test")
		dir, err := Runtime()
		if err != nil {
			t.Fatalf("Runtime() error: %v", err)
		}
		want := "/tmp-test/runtime-" + strconv.Itoa(os.Getuid())
		if dir != want {
			t.Errorf("Runtime() = %q, want %q", dir, want)
		}
	})

	t.Run("relative rejected", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "relative/path")
		if _, err := Runtime(); err == nil {
			t.Error("Runtime() succeeded with relative $XDG_RUNTIME_DIR, want error")
		}
	})
}
