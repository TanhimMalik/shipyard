package projects

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Project struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Slug       string    `json:"slug"`
	GitURL     string    `json:"git_url"`
	Branch     string    `json:"branch"`
	AppPort    int       `json:"app_port"`
	HealthPath string    `json:"health_path"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type CreateInput struct {
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	GitURL     string `json:"git_url"`
	Branch     string `json:"branch"`
	AppPort    int    `json:"app_port"`
	HealthPath string `json:"health_path"`
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Validate returns normalized configuration and field-specific errors.
func Validate(input CreateInput) (CreateInput, map[string]string) {
	input.Name = strings.TrimSpace(input.Name)
	input.Slug = strings.TrimSpace(input.Slug)
	input.GitURL = strings.TrimSpace(input.GitURL)
	input.Branch = strings.TrimSpace(input.Branch)
	input.HealthPath = strings.TrimSpace(input.HealthPath)

	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"name", input.Name, 100},
		{"slug", input.Slug, 63},
		{"git_url", input.GitURL, 2048},
		{"branch", input.Branch, 255},
		{"health_path", input.HealthPath, 2048},
	}
	problems := make(map[string]string)
	for _, field := range fields {
		switch {
		case !utf8.ValidString(field.value):
			problems[field.name] = "Must be valid UTF-8."
		case field.value == "":
			problems[field.name] = "Required."
		case strings.IndexFunc(field.value, unicode.IsControl) >= 0:
			problems[field.name] = "Must not contain control characters."
		case utf8.RuneCountInString(field.value) > field.limit:
			problems[field.name] = fmt.Sprintf("Must be at most %d characters.", field.limit)
		}
	}
	if problems["slug"] == "" && !slugPattern.MatchString(input.Slug) {
		problems["slug"] = "Use lowercase letters and numbers, separated by single hyphens."
	}
	if problems["git_url"] == "" && !validGitURL(input.GitURL) {
		problems["git_url"] = "Use an HTTPS repository URL without credentials, a query, or a fragment."
	}
	if problems["branch"] == "" && !validBranch(input.Branch) {
		problems["branch"] = "Use a branch name, without a full ref or revision expression."
	}
	if input.AppPort < 1 || input.AppPort > 65535 {
		problems["app_port"] = "Must be between 1 and 65535."
	}
	if problems["health_path"] == "" && !validHealthPath(input.HealthPath) {
		problems["health_path"] = "Use a path beginning with /, without a query, fragment, or path traversal."
	}
	if len(problems) == 0 {
		return input, nil
	}
	return input, problems
}

func validGitURL(value string) bool {
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
		u.Opaque == "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery &&
		!strings.Contains(value, "#") && strings.Trim(u.Path, "/") != ""
}

// Branches use Git's ref-name rules, with one-level names allowed. A later
// checkout must fetch refs/heads/<name>, never resolve the input as a revision.
func validBranch(value string) bool {
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

func validHealthPath(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.ContainsAny(value, "?#\\") ||
		strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return false
	}
	u, err := url.ParseRequestURI(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasPrefix(u.Path, "/") ||
		strings.HasPrefix(u.Path, "//") || strings.ContainsRune(u.Path, '\\') ||
		strings.IndexFunc(u.Path, unicode.IsControl) >= 0 || strings.Count(u.Path, "/") != strings.Count(value, "/") {
		return false
	}
	// Reject encoded separators and decoded dot-segments instead of normalizing
	// them into a different endpoint. Benign escapes remain unchanged.
	for _, component := range strings.Split(u.Path, "/") {
		if component == "." || component == ".." {
			return false
		}
	}
	return true
}
