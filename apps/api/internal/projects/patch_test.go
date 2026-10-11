package projects

import "testing"

func TestValidatePatchOnlyChecksSuppliedFields(t *testing.T) {
	input := PatchInput{Name: patchValue("  Renamed project  ")}
	normalized, fields := ValidatePatch(input)
	if fields != nil || normalized.Name == nil || *normalized.Name != "Renamed project" {
		t.Fatalf("partial validation = %+v, %v", normalized, fields)
	}
	if *input.Name != "  Renamed project  " {
		t.Fatal("validation modified the caller's input")
	}
	if normalized.Slug != nil || normalized.GitURL != nil || normalized.Branch != nil ||
		normalized.AppPort != nil || normalized.HealthPath != nil {
		t.Fatal("omitted fields became replacements")
	}
}

func TestValidatePatchNormalizesEveryField(t *testing.T) {
	input := PatchInput{
		Name: patchValue(" App "), Slug: patchValue(" renamed-app "),
		GitURL: patchValue(" https://example.com/app.git "), Branch: patchValue(" feature/next "),
		AppPort: patchValue(8081), HealthPath: patchValue(" /ready "),
	}
	normalized, fields := ValidatePatch(input)
	if fields != nil {
		t.Fatalf("valid patch rejected: %v", fields)
	}
	if *normalized.Name != "App" || *normalized.Slug != "renamed-app" ||
		*normalized.GitURL != "https://example.com/app.git" || *normalized.Branch != "feature/next" ||
		*normalized.AppPort != 8081 || *normalized.HealthPath != "/ready" {
		t.Fatalf("incorrectly normalized patch: %+v", normalized)
	}
}

func TestValidatePatchRejectsProvidedInvalidValues(t *testing.T) {
	for _, test := range []struct {
		field string
		input PatchInput
	}{
		{"name", PatchInput{Name: patchValue(" ")}},
		{"slug", PatchInput{Slug: patchValue("Invalid")}},
		{"git_url", PatchInput{GitURL: patchValue("http://example.com/app.git")}},
		{"branch", PatchInput{Branch: patchValue("main~1")}},
		{"app_port", PatchInput{AppPort: patchValue(0)}},
		{"health_path", PatchInput{HealthPath: patchValue("//example.com/ready")}},
	} {
		t.Run(test.field, func(t *testing.T) {
			_, fields := ValidatePatch(test.input)
			if len(fields) != 1 || fields[test.field] == "" {
				t.Fatalf("validation errors = %v, want only %s", fields, test.field)
			}
		})
	}
}

func patchValue[T any](value T) *T { return &value }
