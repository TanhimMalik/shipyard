package source

import (
	"regexp"
	"strconv"
)

var gitVersionPattern = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)(?:\.|\s|$)`)

func supportedGitVersion(value string) bool {
	match := gitVersionPattern.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(match[2])
	return err == nil && (major > 2 || major == 2 && minor >= 39)
}
