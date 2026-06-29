package ssh

import (
	"slices"
	"strconv"
	"testing"
)

func TestBinaryArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   Target
		want []string
	}{
		{
			name: "ssh config name",
			in:   Target{Name: "prod"},
			want: []string{"prod"},
		},
		{
			name: "explicit host only",
			in:   Target{Host: "host"},
			want: []string{"host"},
		},
		{
			name: "explicit default port",
			in:   Target{User: "alice", Host: "host", Port: 22},
			want: []string{"alice@host"},
		},
		{
			name: "explicit custom port",
			in:   Target{User: "alice", Host: "host", Port: 2222},
			want: []string{"-p", "2222", "alice@host"},
		},
		{
			name: "name wins",
			in:   Target{Name: "prod", User: "alice", Host: "host", Port: 2222},
			want: []string{"prod"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BinaryArgs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestCommandArgs_Flags asserts the one-shot command path carries the
// non-interactive + keepalive flags that the SFTP/probe paths do not need to:
// -n, -o BatchMode=yes, and ServerAlive keepalives (decision D16).
func TestCommandArgs_Flags(t *testing.T) {
	t.Parallel()

	got := CommandArgs(Target{Name: "prod"})

	// Target selection still comes first.
	if len(got) == 0 || got[0] != "prod" {
		t.Fatalf("expected target 'prod' first, got %v", got)
	}

	wantContains := [][]string{
		{"-n"},
		{"-o", "BatchMode=yes"},
		{"-o", "ServerAliveInterval=" + strconv.Itoa(ServerAliveInterval)},
		{"-o", "ServerAliveCountMax=" + strconv.Itoa(ServerAliveCountMax)},
	}
	for _, want := range wantContains {
		if !containsSeq(got, want) {
			t.Errorf("CommandArgs missing %v; got %v", want, got)
		}
	}
}

// TestCommandArgs_PreservesTarget ensures port/user selection from BinaryArgs
// is preserved ahead of the added flags.
func TestCommandArgs_PreservesTarget(t *testing.T) {
	t.Parallel()
	got := CommandArgs(Target{User: "alice", Host: "host", Port: 2222})
	if !containsSeq(got, []string{"-p", "2222", "alice@host"}) {
		t.Errorf("CommandArgs lost target selection; got %v", got)
	}
	if !containsSeq(got, []string{"-o", "BatchMode=yes"}) {
		t.Errorf("CommandArgs missing BatchMode; got %v", got)
	}
}

// containsSeq reports whether sub appears as a contiguous subsequence of s.
func containsSeq(s, sub []string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if slices.Equal(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}
