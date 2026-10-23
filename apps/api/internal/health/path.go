package health

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidPath accepts an application path without changing its endpoint or origin.
func ValidPath(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 2048 ||
		!strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.ContainsAny(value, "?#\\") || strings.IndexFunc(value, unicode.IsSpace) >= 0 ||
		strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	u, err := url.ParseRequestURI(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !utf8.ValidString(u.Path) ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") ||
		strings.ContainsRune(u.Path, '\\') || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 ||
		strings.Count(u.Path, "/") != strings.Count(value, "/") {
		return false
	}
	// Reject encoded separators and dot segments; preserve benign escapes.
	for _, component := range strings.Split(u.Path, "/") {
		if component == "." || component == ".." {
			return false
		}
	}
	return true
}
