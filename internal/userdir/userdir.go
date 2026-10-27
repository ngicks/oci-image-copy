// Package userdir resolves the per-user base directories the Go standard
// library has no resolver for: data ($XDG_DATA_HOME), state
// ($XDG_STATE_HOME), and runtime ($XDG_RUNTIME_DIR). Each function mirrors
// the per-GOOS structure of os.UserConfigDir / os.UserCacheDir (XDG env vars
// honored on Unix only) and, like them, returns the base directory — the
// caller appends its own per-app subdirectory, e.g.
// filepath.Join(dir, appName), with appName spelled identically to the
// service package's defaultConfigDir.
//
// Resolvers only compute paths — nothing is created. Create the per-app
// subdirectory at first write with os.MkdirAll(dir, 0o700); the runtime
// subdirectory MUST stay 0700.
package userdir

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// Data returns the base directory for durable, user-valued files the app
// manages. Unix: $XDG_DATA_HOME, else ~/.local/share. macOS:
// ~/Library/Application Support. Windows: %AppData%.
func Data() (string, error) {
	var dir string
	switch runtime.GOOS {
	case "windows":
		dir = os.Getenv("AppData")
		if dir == "" {
			return "", errors.New("%AppData% is not defined")
		}
	case "darwin", "ios":
		dir = os.Getenv("HOME")
		if dir == "" {
			return "", errors.New("$HOME is not defined")
		}
		dir += "/Library/Application Support"
	default: // Unix
		dir = os.Getenv("XDG_DATA_HOME")
		if dir == "" {
			dir = os.Getenv("HOME")
			if dir == "" {
				return "", errors.New("neither $XDG_DATA_HOME nor $HOME are defined")
			}
			dir += "/.local/share"
		} else if !filepath.IsAbs(dir) {
			return "", errors.New("path in $XDG_DATA_HOME is relative")
		}
	}
	return dir, nil
}

// State returns the base directory for persisted but re-creatable app state
// (logs, history, cursors). Unix: $XDG_STATE_HOME, else ~/.local/state.
// macOS: ~/Library/Application Support (Caches is OS-purgeable; state must
// survive). Windows: %LocalAppData% (machine-local — state should not roam).
func State() (string, error) {
	var dir string
	switch runtime.GOOS {
	case "windows":
		dir = os.Getenv("LocalAppData")
		if dir == "" {
			return "", errors.New("%LocalAppData% is not defined")
		}
	case "darwin", "ios":
		dir = os.Getenv("HOME")
		if dir == "" {
			return "", errors.New("$HOME is not defined")
		}
		dir += "/Library/Application Support"
	default: // Unix
		dir = os.Getenv("XDG_STATE_HOME")
		if dir == "" {
			dir = os.Getenv("HOME")
			if dir == "" {
				return "", errors.New("neither $XDG_STATE_HOME nor $HOME are defined")
			}
			dir += "/.local/state"
		} else if !filepath.IsAbs(dir) {
			return "", errors.New("path in $XDG_STATE_HOME is relative")
		}
	}
	return dir, nil
}

// Runtime returns the base directory for sockets, pipes, and lock files
// scoped to the login session. Unix: $XDG_RUNTIME_DIR — the XDG spec defines
// NO default (/run/user/$UID is provided by systemd at login, not a fallback
// an app may assume).
//
// When $XDG_RUNTIME_DIR is unset, and always on macOS/Windows (no
// session-runtime equivalent), it returns a degraded fallback instead of an
// error: os.TempDir()/runtime-<uid> (the uid suffix disambiguates the shared
// /tmp on Unix; Getuid is -1 on Windows — harmless, TempDir is already
// per-user there). WARNING — the fallback is NOT session-scoped: nothing
// clears it at logout, so remove what you create (defer os.Remove(sock)),
// and on shared /tmp another user can pre-create the uid directory, which
// os.MkdirAll silently accepts — verify ownership/mode if that matters.
// A feature that requires true session scoping must check $XDG_RUNTIME_DIR /
// runtime.GOOS itself and fail; Runtime will not error for it.
//
// The per-app subdirectory appended to the result MUST be created 0700.
func Runtime() (string, error) {
	switch runtime.GOOS {
	case "windows", "darwin", "ios":
		return runtimeFallback(), nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return runtimeFallback(), nil
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("path in $XDG_RUNTIME_DIR is relative")
	}
	return dir, nil
}

// runtimeFallback is Runtime's degraded base: a per-uid subdirectory of
// os.TempDir(), disambiguating the shared /tmp on Unix.
func runtimeFallback() string {
	return filepath.Join(os.TempDir(), "runtime-"+strconv.Itoa(os.Getuid()))
}
