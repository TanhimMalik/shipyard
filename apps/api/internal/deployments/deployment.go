package deployments

import "time"

const (
	StatusQueued         = "QUEUED"
	StatusFetchingSource = "FETCHING_SOURCE"
	StatusFailed         = "FAILED"
)

type Deployment struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	GitURL        string     `json:"git_url"`
	Branch        string     `json:"branch"`
	AppPort       int        `json:"app_port"`
	HealthPath    string     `json:"health_path"`
	Status        string     `json:"status"`
	CommitSHA     *string    `json:"commit_sha"`
	FailureStage  *string    `json:"failure_stage"`
	FailureReason *string    `json:"failure_reason"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Claim identifies one executor's ownership of an attempt. The token never
// appears in API responses and becomes invalid when the attempt fails.
type Claim struct {
	Deployment
	Token string `json:"-"`
}
