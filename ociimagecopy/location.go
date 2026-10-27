package ociimagecopy

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

var ErrInvalidLocation = errors.New("ociimagecopy: invalid location")

type Kind int

const (
	KindStore Kind = iota + 1
	KindContainersStorage
)

const (
	storePrefix             = "store:"
	containersStoragePrefix = "containers-storage:"
)

// Location is a parsed SRC or DST: store:/path, store:s3://bucket/prefix,
// store:http(s)://host/base or containers-storage:.
type Location struct {
	Kind Kind
	// URL locates a KindStore location; its scheme is file, s3, http or https.
	// A file URL holds an absolute path.
	URL *url.URL
}

// ParseLocation parses s; a relative store path is made absolute against the working directory.
func ParseLocation(s string) (Location, error) {
	if s == containersStoragePrefix {
		return Location{Kind: KindContainersStorage}, nil
	}
	if strings.HasPrefix(s, containersStoragePrefix) {
		return Location{}, fmt.Errorf(
			"%w: %q: containers-storage takes no store specification (storage.conf selects it)",
			ErrInvalidLocation,
			s,
		)
	}
	rest, ok := strings.CutPrefix(s, storePrefix)
	if !ok || rest == "" {
		return Location{}, fmt.Errorf(
			"%w: %q: want store:<path|s3://bucket/prefix|http(s)://host/base> or %s",
			ErrInvalidLocation,
			s,
			containersStoragePrefix,
		)
	}
	var u *url.URL
	scheme, _, isURL := strings.Cut(rest, "://")
	isURL = isURL && !strings.ContainsAny(scheme, `/\`)
	// file:/path is the URI form without authority. Any other "x:" prefix stays part of a path,
	// as a Windows drive letter does.
	if isURL || strings.HasPrefix(rest, "file:") {
		var err error
		u, err = url.Parse(rest)
		if err != nil {
			return Location{}, fmt.Errorf("%w: %q: %w", ErrInvalidLocation, s, err)
		}
		if u.Scheme == "file" {
			if (u.Host != "" && u.Host != "localhost") || u.User != nil || u.RawQuery != "" ||
				u.Fragment != "" || u.ForceQuery || u.Opaque != "" {
				return Location{}, fmt.Errorf(
					"%w: %q: a file URL takes only a local absolute path",
					ErrInvalidLocation,
					s,
				)
			}
			u = &url.URL{Scheme: "file", Path: u.Path}
		}
	} else {
		u = &url.URL{Scheme: "file", Path: rest}
	}
	if u.Scheme == "file" && u.Path != "" {
		abs, err := filepath.Abs(filepath.FromSlash(u.Path))
		if err != nil {
			return Location{}, fmt.Errorf("%w: %q: %w", ErrInvalidLocation, s, err)
		}
		u.Path = filepath.ToSlash(abs)
	}
	l := Location{Kind: KindStore, URL: u}
	if err := l.validate(); err != nil {
		return Location{}, err
	}
	return l, nil
}

func (l Location) String() string {
	switch l.Kind {
	case KindContainersStorage:
		return containersStoragePrefix
	case KindStore:
		if l.URL == nil {
			return storePrefix
		}
		if l.URL.Scheme == "file" {
			return storePrefix + filepath.FromSlash(l.URL.Path)
		}
		return storePrefix + l.URL.String()
	}
	return fmt.Sprintf("invalid location kind %d", l.Kind)
}

// Writable reports whether l can be a destination; http(s) stores are read-only.
func (l Location) Writable() bool {
	if l.validate() != nil {
		return false
	}
	return l.Kind == KindContainersStorage || l.URL.Scheme == "file" || l.URL.Scheme == "s3"
}

// validate rechecks what ParseLocation guarantees, because Location is constructible by hand.
func (l Location) validate() error {
	switch l.Kind {
	case KindContainersStorage:
		return nil
	case KindStore:
	default:
		return fmt.Errorf("%w: unknown kind %d", ErrInvalidLocation, l.Kind)
	}
	u := l.URL
	if u == nil {
		return fmt.Errorf("%w: store location without a URL", ErrInvalidLocation)
	}
	switch u.Scheme {
	case "file":
		if !filepath.IsAbs(filepath.FromSlash(u.Path)) {
			return fmt.Errorf("%w: %s: path is not absolute", ErrInvalidLocation, l)
		}
	case "s3":
		if u.Host == "" {
			return fmt.Errorf("%w: %s: no bucket", ErrInvalidLocation, l)
		}
		if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf(
				"%w: %s: query, fragment and user info are not supported",
				ErrInvalidLocation,
				l,
			)
		}
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("%w: %s: no host", ErrInvalidLocation, l)
		}
		// Store paths are appended to the base as text, so they would land inside a query or
		// fragment.
		if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return fmt.Errorf(
				"%w: %s: query and fragment are not supported",
				ErrInvalidLocation,
				l,
			)
		}
	default:
		return fmt.Errorf("%w: %s: unsupported scheme %q", ErrInvalidLocation, l, u.Scheme)
	}
	return nil
}

// s3Location splits an s3 URL into the bucket and a key prefix ending in "/" unless empty.
func s3Location(u *url.URL) (bucket, prefix string) {
	prefix = strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	return u.Host, prefix
}
