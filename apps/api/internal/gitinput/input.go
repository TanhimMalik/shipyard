package gitinput

import (
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidURL accepts HTTPS repository locations without credentials or URL extras.
func ValidURL(value string) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 2048 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	return strings.EqualFold(u.Scheme, "https") && u.Hostname() != "" &&
		validHost(u.Hostname()) && u.Opaque == "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery &&
		!strings.Contains(value, "#") && strings.Trim(u.Path, "/") != ""
}

// ValidBranch accepts Git branch names, including one-level names. Checkouts
// must fetch refs/heads/<name>, never resolve the input as a revision.
func ValidBranch(value string) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 255 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	if value == "HEAD" || value == "@" || strings.HasPrefix(value, "refs/") ||
		strings.HasPrefix(value, "-") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") ||
		strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

var dnsHostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)
var numericHostLabel = regexp.MustCompile(`^(?:[0-9]+|0[xX][0-9A-Fa-f]+)$`)

func validHost(host string) bool {
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Zone() == ""
	}
	if len(host) > 253 || !dnsHostPattern.MatchString(host) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	// libcurl accepts legacy numeric IPv4 forms that Go's IP parser rejects.
	// Do not let DNS checking and the actual connection interpret different hosts.
	lastLabel := host[strings.LastIndex(host, ".")+1:]
	if numericHostLabel.MatchString(lastLabel) {
		return false
	}
	return true
}
