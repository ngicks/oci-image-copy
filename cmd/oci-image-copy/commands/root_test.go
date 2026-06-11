package commands

import (
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
)

func TestValidateRemoteTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   ssh.Target
	}{
		{name: "empty"},
		{name: "name with host", in: ssh.Target{Name: "prod", Host: "host"}},
		{name: "negative port", in: ssh.Target{Host: "host", Port: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRemoteTarget(tc.in); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateRemoteTargetValid(t *testing.T) {
	t.Parallel()
	cases := []ssh.Target{
		{Name: "prod"},
		{Host: "host"},
		{User: "alice", Host: "host", Port: 2222},
	}
	for _, tc := range cases {
		if err := validateRemoteTarget(tc); err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}
