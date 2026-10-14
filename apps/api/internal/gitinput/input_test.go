package gitinput

import (
	"strings"
	"testing"
)

func TestValidURLHostPolicyAndTextBounds(t *testing.T) {
	for _, value := range []string{
		"https://git_repo.example.com/app.git", "https://bücher.example/app.git",
		"https://github.com./owner/repo.git", "https://example.123/app.git",
		"https://127.1/app.git", "https://0x7f000001/app.git",
		"https://example.com/" + strings.Repeat("x", 2048),
		"https://example.com/a\x00b", "https://example.com/" + string([]byte{0xff}),
	} {
		if ValidURL(value) {
			t.Errorf("accepted invalid repository %q", value)
		}
	}
	for _, value := range []string{
		"HTTPS://GitHub.com/owner/repo.git", "https://github.com:443/owner/repo.git/",
		"https://xn--bcher-kva.example/app.git", "https://8.8.8.8/app.git",
		"https://[2606:4700:4700::1111]/app.git",
		"https://example.com/" + strings.Repeat("界", 2028),
	} {
		if !ValidURL(value) {
			t.Errorf("rejected valid repository %q", value)
		}
	}
}

func TestValidBranchTextBounds(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 256), "a\x00b", string([]byte{0xff})} {
		if ValidBranch(value) {
			t.Errorf("accepted invalid branch %q", value)
		}
	}
	if !ValidBranch(strings.Repeat("界", 255)) {
		t.Error("rejected a 255-rune branch")
	}
}

func TestValidHostSyntax(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"git.example.test", true},
		{"GIT.example.test", true},
		{"git-repo.example.test", true},
		{"xn--bcher-kva.example", true},
		{"localhost", true},
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{strings.Repeat("a", 63) + ".example", true},
		{strings.Repeat("a", 64) + ".example", false},
		{"", false},
		{"git.example.test.", false},
		{"git..example.test", false},
		{"-git.example.test", false},
		{"git-.example.test", false},
		{"git_repo.example.test", false},
		{"*.example.test", false},
		{"127.1", false},
		{"0177.0.0.1", false},
		{"0x7f000001", false},
		{"2130706433", false},
		{"0x7f.0.0.1", false},
		{"2606:4700:4700::1111%lo0", false},
		{"git.example.test:443", false},
		{"git.example.test/path", false},
		{"git.example.test\n", false},
		{"bücher.example", false},
		{strings.Repeat("a.", 127) + "a", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := validHost(tt.host); got != tt.want {
				t.Errorf("validHost(%q) = %t, want %t", tt.host, got, tt.want)
			}
		})
	}
}
