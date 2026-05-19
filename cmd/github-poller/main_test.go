package main

import (
	"bytes"
	"testing"
)

// TestRootCmd_StateDirAlias asserts that --state-dir is accepted at the
// root command and resolves to the same target as --state-root.  The
// canonical flag name (per design PR #4) is --state-dir; --state-root is
// kept as a backwards-compatible alias.
func TestRootCmd_StateDirAlias(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "state-dir at root",
			args: []string{"--state-dir", "/tmp/example-dir", "status"},
			want: "/tmp/example-dir",
		},
		{
			name: "state-root at root (backwards compat)",
			args: []string{"--state-root", "/tmp/example-root", "status"},
			want: "/tmp/example-root",
		},
		{
			name: "state-dir on a subcommand",
			args: []string{"status", "--state-dir", "/tmp/sub-dir"},
			want: "/tmp/sub-dir",
		},
		{
			name: "default when neither flag is passed",
			args: []string{"status"},
			want: "state",
		},
		{
			name: "state-dir wins when both are passed",
			args: []string{"--state-root", "/tmp/lose", "--state-dir", "/tmp/win", "status"},
			want: "/tmp/win",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, stateRoot := newRootCmd()
			// status reads state but doesn't touch the network — perfect
			// canary for resolving the flag value.  Swallow output so the
			// test log stays clean.
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%v): %v", tc.args, err)
			}
			if *stateRoot != tc.want {
				t.Errorf("stateRoot got %q want %q", *stateRoot, tc.want)
			}
		})
	}
}

// TestRootCmd_StateDirRejectsUnknownFlag is a sanity check that we
// haven't accidentally turned --state-dir into a wildcard or normalize
// trap that swallows real typos.
func TestRootCmd_StateDirRejectsUnknownFlag(t *testing.T) {
	root, _ := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--state-direlly", "/tmp/x", "status"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown flag --state-direlly; got nil")
	}
}
