package deployments

import "time"

const StatusQueued = "QUEUED"

type Deployment struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	GitURL     string    `json:"git_url"`
	Branch     string    `json:"branch"`
	AppPort    int       `json:"app_port"`
	HealthPath string    `json:"health_path"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
