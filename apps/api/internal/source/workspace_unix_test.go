//go:build darwin || linux

package source

import (
	"os"
	"syscall"
	"testing"
)

func TestWorkspaceOwnerPolicy(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkWorkspaceOwner(info); err != nil {
		t.Fatalf("current user's directory rejected: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("filesystem did not expose Unix ownership")
	}
	foreign := *stat
	// Simulate another owner without chown or elevated test privileges. This
	// also exercises the privileged-service case when the test runs as root.
	foreign.Uid = uint32(os.Geteuid()) ^ 1
	for _, tt := range []struct {
		name string
		sys  any
	}{
		{"foreign owner", &foreign},
		{"missing ownership metadata", nil},
		{"unexpected ownership metadata", struct{}{}},
		{"nil ownership metadata", (*syscall.Stat_t)(nil)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkWorkspaceOwner(sourceOwnerTestInfo{FileInfo: info, system: tt.sys}); err == nil {
				t.Error("workspace accepted without verified service ownership")
			}
		})
	}
}

type sourceOwnerTestInfo struct {
	os.FileInfo
	system any
}

func (i sourceOwnerTestInfo) Sys() any { return i.system }
