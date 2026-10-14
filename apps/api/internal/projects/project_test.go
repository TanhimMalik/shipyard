package projects

import (
	"strings"
	"testing"
)

func validInput() CreateInput {
	return CreateInput{
		Name: "Demo app", Slug: "demo-app", GitURL: "https://git.example.test/team/app.git",
		Branch: "main", AppPort: 8080, HealthPath: "/health",
	}
}

func TestValidateNormalizesConfiguration(t *testing.T) {
	want := validInput()
	input := want
	input.Name = " \t" + input.Name + "\n"
	input.Slug = " " + input.Slug + " "
	input.GitURL = " " + input.GitURL + " "
	input.Branch = " " + input.Branch + " "
	input.HealthPath = " " + input.HealthPath + " "
	got, problems := Validate(input)
	if got != want || problems != nil {
		t.Fatalf("Validate = %#v, %v; want %#v, nil", got, problems, want)
	}
}

func TestValidateTextBoundsAndUTF8(t *testing.T) {
	cases := []struct {
		field string
		limit int
		set   func(*CreateInput, string)
	}{
		{"name", 100, func(i *CreateInput, v string) { i.Name = v }},
		{"slug", 63, func(i *CreateInput, v string) { i.Slug = v }},
		{"git_url", 2048, func(i *CreateInput, v string) { i.GitURL = v }},
		{"branch", 255, func(i *CreateInput, v string) { i.Branch = v }},
		{"health_path", 2048, func(i *CreateInput, v string) { i.HealthPath = v }},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			for _, value := range []string{" \t\n", string([]byte{0xff}), "a\x00b", strings.Repeat("x", tc.limit+1)} {
				input := validInput()
				tc.set(&input, value)
				if _, problems := Validate(input); problems[tc.field] == "" {
					t.Fatalf("accepted invalid %s value", tc.field)
				}
			}
		})
	}
	input := validInput()
	input.Name = strings.Repeat("界", 100)
	if _, problems := Validate(input); problems != nil {
		t.Fatalf("rejected 100-rune Unicode name: %v", problems)
	}
}

func TestValidateRepositoryURLs(t *testing.T) {
	for _, value := range []string{
		"https://git.example.test/app", "HTTPS://git.example.test/team/app.git", "https://git.example.test:8443/app",
	} {
		input := validInput()
		input.GitURL = value
		if _, problems := Validate(input); problems != nil {
			t.Errorf("rejected %q: %v", value, problems)
		}
	}
	for _, value := range []string{
		"http://git.example.test/app", "git@git.example.test:team/app.git", "/team/app.git",
		"https:///app", "https://git.example.test", "https://git.example.test///",
		"https://user:secret@git.example.test/app", "https://git.example.test/app?token=secret",
		"https://git.example.test/app?", "https://git.example.test/app#", "https://git.example.test/a\nb",
		"https://git.example.test/a b", "https://git.example.test/a\u0085b",
		"https://git.example.test:0/app", "https://git.example.test:65536/app", "https://git.example.test:/app",
		"https://git_repo.example.com/app.git", "https://bücher.example/app.git",
		"https://github.com./owner/repo.git", "https://example.123/app.git",
	} {
		input := validInput()
		input.GitURL = value
		if _, problems := Validate(input); problems["git_url"] == "" {
			t.Errorf("accepted invalid Git URL %q", value)
		}
	}
}

func TestValidateBranches(t *testing.T) {
	for _, value := range []string{"main", "feature/new-ui", "release/v1.2", "fix/小さい", "topic/-nested", "deadbeef", strings.Repeat("a", 40)} {
		input := validInput()
		input.Branch = value
		if _, problems := Validate(input); problems != nil {
			t.Errorf("rejected branch %q: %v", value, problems)
		}
	}
	for _, value := range []string{
		"HEAD", "@", "@{-1}", "refs/heads/main", "refs/tags/v1",
		"-main", "/main", "main/", "a//b", ".main", "a/.b", "main.lock", "a.lock/b", "main.",
		"a..b", "main~1", "main^", "a:b", "a?b", "a*b", "a[b", "a\\b", "a b", "a\tb",
	} {
		input := validInput()
		input.Branch = value
		if _, problems := Validate(input); problems["branch"] == "" {
			t.Errorf("accepted invalid branch %q", value)
		}
	}
}

func TestValidateHealthPaths(t *testing.T) {
	for _, value := range []string{"/", "/health", "/health/live", "/he%61lth", "/health%20check", "/健康"} {
		input := validInput()
		input.HealthPath = value
		if _, problems := Validate(input); problems != nil {
			t.Errorf("rejected path %q: %v", value, problems)
		}
	}
	for _, value := range []string{
		"health", "//other.example/health", "https://other.example/health", "/health?ready=1", "/health?", "/health#",
		"/a\\b", "/a\nb", "/a b", "/%00", "/%0d%0a", "/%5cother", "/%2fother", "/health%2flive",
		"/a/../health", "/./health", "/%2e%2e/health", "/%zz",
	} {
		input := validInput()
		input.HealthPath = value
		if _, problems := Validate(input); problems["health_path"] == "" {
			t.Errorf("accepted unsafe path %q", value)
		}
	}
}

func TestValidateSlugAndPort(t *testing.T) {
	for _, value := range []string{"Upper", "-app", "app-", "app--one", "app_name", "app.name", "应用"} {
		input := validInput()
		input.Slug = value
		if _, problems := Validate(input); problems["slug"] == "" {
			t.Errorf("accepted invalid slug %q", value)
		}
	}
	for _, value := range []int{0, -1, 65536} {
		input := validInput()
		input.AppPort = value
		if _, problems := Validate(input); problems["app_port"] == "" {
			t.Errorf("accepted invalid port %d", value)
		}
	}
	for _, value := range []int{1, 65535} {
		input := validInput()
		input.AppPort = value
		if _, problems := Validate(input); problems != nil {
			t.Errorf("rejected boundary port %d: %v", value, problems)
		}
	}
}
