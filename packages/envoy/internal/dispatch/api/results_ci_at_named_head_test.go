// results_ci_at_named_head_test.go holds GET /api/v1/issues/{key}/results?at= to the --at clause of
// R8 of dispatch://INFRA-4303/spec (approved v3): "Where `--at` names the head of an open or merged
// pull request of the issue, that head is the one whose CI run the check reads" (ciHeadsOf,
// namedCIHeads). Each case is red at main ac9e87136e, where the read takes the open pull request of
// the highest number whatever commit the reader named.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// readAt is c1 as the results read pinned to commit answers it.
func (r ciRepositoryRig) readAt(t *testing.T, commit string) ciRepositoryRead {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+r.issue+"/results?at="+commit, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s at %s: status=%d body=%s", r.issue, commit, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID string `json:"block_id"`
			ciRepositoryRead
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s at %s: %v", r.issue, commit, err)
	}
	for _, check := range read.Checks {
		if check.BlockID == "c1" {
			return check.ciRepositoryRead
		}
	}
	t.Fatalf("the read of %s at %s holds no check c1: %s", r.issue, commit, response.Body.String())
	return ciRepositoryRead{}
}

// TestCIResultAtANamedOpenHeadIsReadAtThatHead: #70 is open at H1, where go-checks passed, and #71
// is open at H2, where it failed. A read with ?at=H1 names #70's head, so the check's own CI read
// is H1's and names the result H1's run derived; no result derived at H2 is the check's.
func TestCIResultAtANamedOpenHeadIsReadAtThatHead(t *testing.T) {
	t.Parallel()
	rig := newCIRepositoryRig(t)
	rig.write("acme/widgets", 70, ciEachHeadH1)
	rig.write("acme/widgets", 71, ciEachHeadH2)
	rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
	rig.github.set("acme/widgets", ciEachHeadH2, completedRun(64, "go-checks", "failure"))
	read := rig.readAt(t, ciEachHeadH1)

	ciReadWants(t, "c1's own read at ?at=H1", read.CI,
		map[string]any{"state": "pass", "case": "success", "commit": ciEachHeadH1, "repo": "acme/widgets", "result": "ci-run:63"})
	for _, result := range read.Results {
		if strings.HasPrefix(result.ID, ciResultIDPrefix) && result.CommitSHA != ciEachHeadH1 {
			t.Errorf("c1 read at ?at=H1 holds %s, derived at %s, a head the read did not name", result.ID, result.CommitSHA)
		}
	}
	if read.PassStands.Answer != "yes" {
		t.Errorf("c1 read at ?at=H1: pass_stands %q (%q), want yes", read.PassStands.Answer, read.PassStands.Reason)
	}
}

// TestANamedHeadIsDecidedByItsOwnCIRead: with ?at= there are no subjects, and passStandsOf asks
// whether the check's own CI read was unread (results_pass_stands.go:63). That read must be the
// named head's, never the read of the open pull request of the highest number.
func TestANamedHeadIsDecidedByItsOwnCIRead(t *testing.T) {
	t.Parallel()
	t.Run("another head unread", func(t *testing.T) {
		t.Parallel()
		rig := newCIRepositoryRig(t)
		rig.write("acme/widgets", 70, ciEachHeadH1)
		rig.write("acme/widgets", 71, ciEachHeadH2)
		rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
		// H2 holds no runs: the stand-in answers 422, so #71's read is unread.
		read := rig.readAt(t, ciEachHeadH1)
		if read.PassStands.Answer != "yes" {
			t.Errorf("?at=H1, whose run passed, while #71's read failed: pass_stands %q (%q), want yes",
				read.PassStands.Answer, read.PassStands.Reason)
		}
	})
	t.Run("the named head unread", func(t *testing.T) {
		t.Parallel()
		rig := newCIRepositoryRig(t)
		rig.write("acme/widgets", 70, ciEachHeadH1)
		rig.write("acme/widgets", 71, ciEachHeadH2)
		// H1 holds no runs: the stand-in answers 422, so #70's read is unread.
		rig.github.set("acme/widgets", ciEachHeadH2, completedRun(64, "go-checks", "success"))
		read := rig.readAt(t, ciEachHeadH1)
		if read.PassStands.Answer != "unknown" {
			t.Errorf("?at=H1, whose run GitHub could not be read for: pass_stands %q (%q), want unknown",
				read.PassStands.Answer, read.PassStands.Reason)
		}
	})
}

// TestCIResultAtANamedMergedHeadIsReadAtThatHead: #70 merged at its final head H1, where go-checks
// passed, and #71 is open at H2, where it failed. R8 names the head of a merged pull request as it
// names an open one's, so ?at=H1 reads H1 though an open pull request has another head.
func TestCIResultAtANamedMergedHeadIsReadAtThatHead(t *testing.T) {
	t.Parallel()
	rig := newCIRepositoryRig(t)
	rig.merged("acme/widgets", 70, ciEachHeadH1)
	rig.write("acme/widgets", 71, ciEachHeadH2)
	rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
	rig.github.set("acme/widgets", ciEachHeadH2, completedRun(64, "go-checks", "failure"))
	read := rig.readAt(t, ciEachHeadH1)

	ciReadWants(t, "c1's own read at ?at=H1, #70's final head", read.CI,
		map[string]any{"state": "pass", "case": "success", "commit": ciEachHeadH1, "repo": "acme/widgets", "result": "ci-run:63"})
	if read.PassStands.Answer != "yes" {
		t.Errorf("c1 read at ?at=H1: pass_stands %q (%q), want yes", read.PassStands.Answer, read.PassStands.Reason)
	}
}

// TestANamedCommitThatIsNoHeadReadsAsBefore: where ?at= names a commit no pull request of the issue
// has as its head, R8 leaves the read as it is: the check's own CI read is the open pull request
// of the highest number, #71 at H2.
func TestANamedCommitThatIsNoHeadReadsAsBefore(t *testing.T) {
	t.Parallel()
	rig := newCIRepositoryRig(t)
	rig.write("acme/widgets", 70, ciEachHeadH1)
	rig.write("acme/widgets", 71, ciEachHeadH2)
	rig.github.set("acme/widgets", ciEachHeadH1, completedRun(63, "go-checks", "success"))
	rig.github.set("acme/widgets", ciEachHeadH2, completedRun(64, "go-checks", "failure"))
	read := rig.readAt(t, ciEachHeadH3)

	ciReadWants(t, "c1's own read at ?at=H3, no head of the issue", read.CI,
		map[string]any{"state": "fail", "case": "failure", "commit": ciEachHeadH2, "repo": "acme/widgets", "result": "ci-run:64"})
}
