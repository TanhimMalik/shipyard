package health

import (
	"strings"
	"testing"
)

func TestValidPathMatchesSavedEndpointRules(t *testing.T) {
	for _, path := range []string{"/", "/health/live", "/he%61lth", "/health%20check", "/健康", "/" + strings.Repeat("a", 2047)} {
		if !ValidPath(path) {
			t.Errorf("rejected valid path %q", path)
		}
	}
	for _, path := range []string{
		"", "health", "//other.example/health", "https://other.example/health", "/health?ready=1", "/health?", "/health#",
		"/a\\b", "/a\nb", "/a b", "/%00", "/%0d%0a", "/%5cother", "/%2fother", "/health%2flive",
		"/a/../health", "/./health", "/%2e%2e/health", "/%zz", "/%ff", "/%c0%af", "/\xff", "/" + strings.Repeat("a", 2048),
	} {
		if ValidPath(path) {
			t.Errorf("accepted invalid path %q", path)
		}
	}
}
