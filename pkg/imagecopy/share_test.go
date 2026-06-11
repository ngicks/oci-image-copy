package imagecopy

import (
	"sort"
	"testing"
)

func sorted(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for d := range s {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func digestSet(ds ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(ds))
	for _, d := range ds {
		s[d] = struct{}{}
	}
	return s
}

func Test_mapKeyDiff(t *testing.T) {
	t.Parallel()

	const (
		manifest = "sha256:m"
		config   = "sha256:c"
		l1       = "sha256:l1"
		l2       = "sha256:l2"
		l3       = "sha256:l3"
	)

	cases := []struct {
		name   string
		need   map[string]struct{}
		have   map[string]struct{}
		pinned map[string]struct{}
		want   []string
	}{
		{
			name:   "empty have, all sent (incl pinned)",
			need:   digestSet(manifest, config, l1, l2),
			have:   digestSet(),
			pinned: digestSet(manifest, config),
			want:   []string{config, l1, l2, manifest},
		},
		{
			name:   "full overlap, only pinned sent",
			need:   digestSet(manifest, config, l1, l2),
			have:   digestSet(manifest, config, l1, l2),
			pinned: digestSet(manifest, config),
			want:   []string{config, manifest},
		},
		{
			name:   "partial overlap",
			need:   digestSet(manifest, config, l1, l2, l3),
			have:   digestSet(l1, l2),
			pinned: digestSet(manifest, config),
			want:   []string{config, l3, manifest},
		},
		{
			name:   "pinned digest in have is still emitted",
			need:   digestSet(l1),
			have:   digestSet(manifest, config, l1),
			pinned: digestSet(manifest, config),
			want:   []string{config, manifest},
		},
		{
			name:   "no pinned",
			need:   digestSet(l1, l2),
			have:   digestSet(l1),
			pinned: digestSet(),
			want:   []string{l2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sorted(mapKeyDiff(tc.need, tc.have, tc.pinned))
			sort.Strings(tc.want)
			if !eq(got, tc.want) {
				t.Errorf("Diff: got %v, want %v", got, tc.want)
			}
		})
	}
}
