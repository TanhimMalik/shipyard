package projects

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"shipyard/apps/api/internal/gitinput"
	"shipyard/apps/api/internal/health"
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
	if problems["git_url"] == "" && !gitinput.ValidURL(input.GitURL) {
		problems["git_url"] = "Use an HTTPS repository URL with a valid host, without credentials, a query, or a fragment."
	}
	if problems["branch"] == "" && !gitinput.ValidBranch(input.Branch) {
		problems["branch"] = "Use a branch name, without a full ref or revision expression."
	}
	if input.AppPort < 1 || input.AppPort > 65535 {
		problems["app_port"] = "Must be between 1 and 65535."
	}
	if problems["health_path"] == "" && !health.ValidPath(input.HealthPath) {
		problems["health_path"] = "Use a path beginning with /, without a query, fragment, or path traversal."
	}
	if len(problems) == 0 {
		return input, nil
	}
	return input, problems
}
