package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// parseIssueRepo decodes the tri-state `repo` field of an issue write. Absent → (nil, false): leave
// it alone. JSON null → (nil, true): clear it, so the issue resolves through its parent chain and the
// project's default again. A string → its canonical `owner/name` (externalRef's canonicalisation,
// which lowercases and drops a `.git` suffix); anything else → 400 INVALID_ISSUE.
func parseIssueRepo(raw json.RawMessage) (repo *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", `repo must be "owner/name" or null`)
	}
	canonical, _, err := externalRef(strings.TrimSpace(value) + "#1")
	if err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", `repo must be "owner/name" (got %q)`, value)
	}
	return &canonical, true, nil
}

// repoScan receives refs.IssueRepoColumns for one issue, the repository-resolution rule the
// server keeps in one place: refs.IssueRepository reads the same terms for a single key, for
// the currency mark and the pull-request head attribution, so an issue read and those two
// cannot name different repositories for the same issue.
type repoScan struct {
	own, resolved *string
	tooDeep       bool
}

func (r *repoScan) targets() []any { return []any{&r.own, &r.resolved, &r.tooDeep} }

// resolve answers the issue's own and resolved repository, refusing a parent chain the walk could
// not finish within parentDepthCap links: such a chain names no answer, and the project's default
// would be a wrong one.
func (r *repoScan) resolve(key string) (own, resolved *string, err error) {
	if r.tooDeep {
		return nil, nil, fmt.Errorf("resolve the repository of %s: its parent chain is deeper than %d links or loops", key, parentDepthCap)
	}
	return r.own, r.resolved, nil
}
