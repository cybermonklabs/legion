package model

import "time"

// Deploy is one recorded build of one deployed program. A nil RunAttempt means that
// GitHub has not supplied an attempt, not attempt one. Route records have no run.
type Deploy struct {
	ID         int64     `json:"id"`
	Repo       string    `json:"repo"`
	Program    string    `json:"program"`
	Commit     string    `json:"commit"`
	DeployedAt time.Time `json:"deployed_at"`
	Source     string    `json:"source" enum:"delivery_run,route"`
	RunID      *int64    `json:"run_id,omitempty"`
	RunAttempt *int      `json:"run_attempt"`
	URL        string    `json:"url"`
	RecordedAt time.Time `json:"recorded_at"`
	RecordedBy Actor     `json:"recorded_by"`
}

// CheckDeploy is the current build a live check reads, beside its branch bases.
type CheckDeploy struct {
	Program    string    `json:"program"`
	Commit     string    `json:"commit"`
	DeployedAt time.Time `json:"deployed_at"`
}
