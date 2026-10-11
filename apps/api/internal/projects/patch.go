package projects

type PatchInput struct {
	Name       *string `json:"name"`
	Slug       *string `json:"slug"`
	GitURL     *string `json:"git_url"`
	Branch     *string `json:"branch"`
	AppPort    *int    `json:"app_port"`
	HealthPath *string `json:"health_path"`
}

func (input PatchInput) empty() bool {
	return input.Name == nil && input.Slug == nil && input.GitURL == nil &&
		input.Branch == nil && input.AppPort == nil && input.HealthPath == nil
}

func ValidatePatch(input PatchInput) (PatchInput, map[string]string) {
	var values CreateInput
	if input.Name != nil {
		values.Name = *input.Name
	}
	if input.Slug != nil {
		values.Slug = *input.Slug
	}
	if input.GitURL != nil {
		values.GitURL = *input.GitURL
	}
	if input.Branch != nil {
		values.Branch = *input.Branch
	}
	if input.AppPort != nil {
		values.AppPort = *input.AppPort
	}
	if input.HealthPath != nil {
		values.HealthPath = *input.HealthPath
	}

	// Reuse creation rules, retaining errors and normalized values only for
	// fields supplied by the caller. Omitted fields never become replacements.
	normalized, fields := Validate(values)
	if input.Name == nil {
		delete(fields, "name")
	} else {
		input.Name = &normalized.Name
	}
	if input.Slug == nil {
		delete(fields, "slug")
	} else {
		input.Slug = &normalized.Slug
	}
	if input.GitURL == nil {
		delete(fields, "git_url")
	} else {
		input.GitURL = &normalized.GitURL
	}
	if input.Branch == nil {
		delete(fields, "branch")
	} else {
		input.Branch = &normalized.Branch
	}
	if input.AppPort == nil {
		delete(fields, "app_port")
	} else {
		input.AppPort = &normalized.AppPort
	}
	if input.HealthPath == nil {
		delete(fields, "health_path")
	} else {
		input.HealthPath = &normalized.HealthPath
	}
	if len(fields) == 0 {
		return input, nil
	}
	return input, fields
}
