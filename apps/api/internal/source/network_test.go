package source

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicAddressPolicy(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    bool
	}{
		{"public IPv4", "8.8.8.8", true},
		{"public IPv6", "2606:4700:4700::1111", true},
		{"public IPv6 outside IETF assignments", "2001:4860:4860::8888", true},
		{"mapped public IPv4", "::ffff:8.8.8.8", true},
		{"unspecified IPv4", "0.0.0.0", false},
		{"this network", "0.10.20.30", false},
		{"private class A", "10.20.30.40", false},
		{"private class B", "172.31.255.255", false},
		{"private class C", "192.168.1.1", false},
		{"loopback", "127.20.30.40", false},
		{"link local metadata", "169.254.169.254", false},
		{"before CGNAT", "100.63.255.255", true},
		{"CGNAT start", "100.64.0.0", false},
		{"CGNAT end", "100.127.255.255", false},
		{"after CGNAT", "100.128.0.0", true},
		{"IETF assignment", "192.0.0.8", false},
		{"documentation first range", "192.0.2.1", false},
		{"documentation second range", "198.51.100.1", false},
		{"documentation third range", "203.0.113.1", false},
		{"deprecated 6to4 relay", "192.88.99.1", false},
		{"benchmark start", "198.18.0.0", false},
		{"benchmark end", "198.19.255.255", false},
		{"after benchmark", "198.20.0.0", true},
		{"multicast IPv4", "224.0.0.1", false},
		{"reserved IPv4", "240.0.0.1", false},
		{"broadcast", "255.255.255.255", false},
		{"mapped loopback", "::ffff:127.0.0.1", false},
		{"mapped private IPv4", "::ffff:10.0.0.1", false},
		{"mapped CGNAT", "::ffff:100.64.0.1", false},
		{"mapped documentation", "::ffff:192.0.2.1", false},
		{"unspecified IPv6", "::", false},
		{"loopback IPv6", "::1", false},
		{"unique local first half", "fc00::1", false},
		{"unique local second half", "fd00::1", false},
		{"link local IPv6", "fe80::1", false},
		{"deprecated site local IPv6", "fec0::1", false},
		{"multicast IPv6", "ff02::1", false},
		{"documentation IPv6", "2001:db8::1", false},
		{"new documentation IPv6", "3fff::1", false},
		{"IPv6 benchmark", "2001:2::1", false},
		{"Teredo", "2001:0:4136:e378:8000:63bf:3fff:fdd2", false},
		{"6to4 containing loopback", "2002:7f00:1::", false},
		{"well known NAT64 containing loopback", "64:ff9b::7f00:1", false},
		{"local NAT64 containing private IPv4", "64:ff9b:1::a00:1", false},
		{"discard only", "100::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address := netip.MustParseAddr(tt.address)
			if got := publicAddress(address); got != tt.want {
				t.Errorf("publicAddress(%s) = %t, want %t", address, got, tt.want)
			}
		})
	}
	if publicAddress(netip.Addr{}) {
		t.Error("publicAddress(invalid address) = true, want false")
	}
}

func TestHostResolutionPinsCheckedAddresses(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		addresses  []string
		want       string
		wantError  bool
	}{
		{"IPv4 and default HTTPS port", "https://git.example.test/app.git", []string{"8.8.8.8"}, "git.example.test:443:8.8.8.8", false},
		{"IPv6 needs brackets", "https://git.example.test/app.git", []string{"2606:4700:4700::1111"}, "git.example.test:443:[2606:4700:4700::1111]", false},
		{"explicit port", "https://git.example.test:8443/app.git", []string{"8.8.8.8"}, "git.example.test:8443:8.8.8.8", false},
		{"all checked addresses", "https://git.example.test/app.git", []string{"1.1.1.1", "8.8.8.8"}, "git.example.test:443:1.1.1.1,8.8.8.8", false},
		{"deduplicate mapped equivalents", "https://git.example.test/app.git", []string{"8.8.8.8", "::ffff:8.8.8.8", "2606:4700:4700::1111"}, "git.example.test:443:8.8.8.8,[2606:4700:4700::1111]", false},
		{"mapped public address is normalized", "https://git.example.test/app.git", []string{"::ffff:8.8.8.8"}, "git.example.test:443:8.8.8.8", false},
		{"literal IPv4 needs no DNS pin", "https://8.8.8.8/app.git", []string{"8.8.8.8"}, "", false},
		{"literal IPv6 needs no DNS pin", "https://[2606:4700:4700::1111]/app.git", []string{"2606:4700:4700::1111"}, "", false},
		{"empty answer", "https://git.example.test/app.git", nil, "", true},
		{"private answer", "https://git.example.test/app.git", []string{"10.0.0.1"}, "", true},
		{"mixed public and private answers", "https://git.example.test/app.git", []string{"8.8.8.8", "127.0.0.1"}, "", true},
		{"mixed address families with private answer", "https://git.example.test/app.git", []string{"2606:4700:4700::1111", "fd00::1"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.repository)
			if err != nil {
				t.Fatal(err)
			}
			addresses := make([]netip.Addr, 0, len(tt.addresses))
			for _, address := range tt.addresses {
				addresses = append(addresses, netip.MustParseAddr(address))
			}
			got, err := hostResolution(u, addresses)
			if (err != nil) != tt.wantError || got != tt.want {
				t.Errorf("hostResolution() = %q, %v; want %q, error %t", got, err, tt.want, tt.wantError)
			}
		})
	}
}

func TestPinHostPreservesCancellation(t *testing.T) {
	u, err := url.Parse("https://git.example.invalid/app.git")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pinHost(ctx, u); !errors.Is(err, context.Canceled) {
		t.Errorf("pinHost with canceled context = %v, want context.Canceled", err)
	}
}

func TestNewFetcherConfigPolicy(t *testing.T) {
	stubGit(t)
	const approved = "https://git.example.test/team/app.git"
	base := func(t *testing.T) Config {
		t.Helper()
		return Config{WorkspaceRoot: filepath.Join(t.TempDir(), "sources"), AllowedRepositories: []string{approved}}
	}
	t.Run("default timeout and private workspace creation", func(t *testing.T) {
		config := base(t)
		fetcher, err := NewFetcher(config)
		if err != nil {
			t.Fatalf("NewFetcher: %v", err)
		}
		if fetcher.timeout != 2*time.Minute {
			t.Errorf("default timeout = %v, want two minutes", fetcher.timeout)
		}
		info, err := os.Stat(config.WorkspaceRoot)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("workspace is not private: info = %v, error = %v", info, err)
		}
	})
	t.Run("maximum timeout accepted", func(t *testing.T) {
		config := base(t)
		config.Timeout = 10 * time.Minute
		if _, err := NewFetcher(config); err != nil {
			t.Fatalf("maximum timeout: %v", err)
		}
	})
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"missing root", func(c *Config) { c.WorkspaceRoot = "" }},
		{"relative root", func(c *Config) { c.WorkspaceRoot = "sources" }},
		{"negative timeout", func(c *Config) { c.Timeout = -time.Second }},
		{"timeout above maximum", func(c *Config) { c.Timeout = 10*time.Minute + time.Nanosecond }},
		{"missing allowlist", func(c *Config) { c.AllowedRepositories = nil }},
		{"HTTP repository", func(c *Config) { c.AllowedRepositories = []string{"http://git.example.test/app.git"} }},
		{"credentials", func(c *Config) { c.AllowedRepositories = []string{"https://user:secret@git.example.test/app.git"} }},
		{"query", func(c *Config) { c.AllowedRepositories = []string{approved + "?token=value"} }},
		{"fragment", func(c *Config) { c.AllowedRepositories = []string{approved + "#main"} }},
		{"invalid DNS label", func(c *Config) { c.AllowedRepositories = []string{"https://git_repo.example.test/app.git"} }},
		{"trailing DNS dot", func(c *Config) { c.AllowedRepositories = []string{"https://git.example.test./app.git"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := base(t)
			tt.change(&config)
			if _, err := NewFetcher(config); err == nil {
				t.Error("NewFetcher accepted invalid configuration")
			}
		})
	}
	t.Run("existing shared root rejected without chmod", func(t *testing.T) {
		config := base(t)
		if err := os.Mkdir(config.WorkspaceRoot, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(config.WorkspaceRoot, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFetcher(config); err == nil {
			t.Error("NewFetcher accepted a shared workspace root")
		}
		info, err := os.Stat(config.WorkspaceRoot)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("constructor changed existing root permissions: info = %v, error = %v", info, err)
		}
	})
}
