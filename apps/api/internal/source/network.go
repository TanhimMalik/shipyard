package source

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// In addition to private/local addresses, exclude special-use ranges that can
// route to internal services or are not ordinary public unicast destinations.
var excludedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	// Restrict IPv6 to globally allocated unicast; this also excludes NAT64
	// translation ranges that could encode private IPv4 destinations.
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}
	for _, prefix := range excludedNetworks {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func pinHost(ctx context.Context, u *url.URL) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("repository host could not be resolved")
	}
	return hostResolution(u, addresses)
}

func hostResolution(u *url.URL, addresses []netip.Addr) (string, error) {
	if len(addresses) == 0 {
		return "", errors.New("repository host has no addresses")
	}
	for _, address := range addresses {
		if !publicAddress(address) {
			return "", errors.New("repository host must resolve only to public addresses")
		}
	}
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		return "", nil // A literal address cannot be rebound through DNS.
	}
	// Pin every checked address for the entire fetch, including follow-up Git
	// requests. Keep the hostname in the URL for TLS certificate validation.
	values := make([]string, 0, len(addresses))
	seen := make(map[netip.Addr]bool, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if seen[address] {
			continue
		}
		seen[address] = true
		value := address.String()
		if address.Is6() {
			value = "[" + value + "]"
		}
		values = append(values, value)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname() + ":" + port + ":" + strings.Join(values, ","), nil
}
