package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// CheckRun is one check run as GitHub names it: its id, its name and the commit it was made for.
// GitHub names the check run of each job of a workflow run after the job, with the job's matrix
// values in parentheses, so a run's name is the job's identity a result is held to.
type CheckRun struct {
	ID      int64
	Name    string
	HeadSHA string
}

// CheckRun reads one check run of owner/repo by its id (`GET /repos/{owner}/{repo}/check-runs/{id}`,
// with an installation token that reads checks). A 403 or 404 is ErrRepositoryUnreadable, as every
// repository read's is (CheckRepositoryResponse). An answer that is not the run asked for, or that
// names no name or no whole commit, is an error and never a run, since a caller holds a result to
// exactly these facts.
func (c *Client) CheckRun(ctx context.Context, token, owner, repo string, id int64) (CheckRun, error) {
	if c == nil {
		return CheckRun{}, ErrNoAppKey
	}
	repoFull := owner + "/" + repo
	body, _, err := c.readRepositoryJSON(ctx, token, repoPath(owner, repo)+"/check-runs/"+strconv.FormatInt(id, 10), repoFull)
	if err != nil {
		return CheckRun{}, err
	}
	var answer struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return CheckRun{}, fmt.Errorf("decode check run %d of %s: %w", id, repoFull, err)
	}
	if answer.ID != id || answer.Name == "" || !commitSHA.MatchString(answer.HeadSHA) {
		return CheckRun{}, fmt.Errorf("check run %d of %s: GitHub answered the run %d named %q at %q, not the run asked for with a name and a whole commit",
			id, repoFull, answer.ID, answer.Name, answer.HeadSHA)
	}
	return CheckRun{ID: answer.ID, Name: answer.Name, HeadSHA: answer.HeadSHA}, nil
}
