package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestAParentStatusConflictNamesAndCountsNoIssueItsWriterDoesNotSee (the live reviewer's F11):
// with the parent status rule on, R24 decides with what its writer knows, as the open-child check
// of an acceptance does (refuseOpenForeignChildren). alice, a full member PRIV's list does not
// name, sees PUB alone. Each write below of a PUB issue whose other end of a pair is PRIV's is
// taken for her, naming that issue nowhere, and leaves it as it was: a 409 would hand her the
// PRIV issue's key, title and status, and a 409 that named none would still tell her an issue she
// may not see is out of line with hers. carol, on PRIV's list, gets the same write of a family of
// her own refused 409 PARENT_STATUS_CONFLICT listing the PRIV issue, so each family breaks the
// rule.
//
//   - a PATCH of a PUB parent to done over an open PRIV child: both of R24's reads of a close
//     (parentStatusConflictOf, and openChildrenOfAClose after the close's update);
//   - a status change of a PUB parent to done over an open PRIV child, and its dry run;
//   - a PATCH of a PUB child of a PRIV parent in triage into work. The PRIV parent hears the
//     child's child.status event, as the parent of every child whose status moves does, so its
//     status and its parent are what stays.
//
// Controls: parentStatusConflictOf's read without its access.ProjectClause, under which each of
// alice's writes is refused 409 listing the PRIV issue (the "want 200" check fails); with that
// read guarded and openChildrenOfAClose's read without its clause, alice's two closes are refused
// 409 by the second read, listing the child, at the same check.
func TestAParentStatusConflictNamesAndCountsNoIssueItsWriterDoesNotSee(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProjectWith(t, testServerOptions{parentStatusRule: true})
	if read := seed.request(t, callerAlice, http.MethodGet, "/api/v1/issues/"+seed.issue[seedPrivate], nil); read.Code != http.StatusNotFound {
		t.Fatalf("alice's read of %s = %d %s, want 404: she sees PRIV, so the case proves nothing", seed.issue[seedPrivate], read.Code, read.Body.String())
	}
	rule := ruleRig{database: seed.database}
	// pair is a PUB issue (own) and the PRIV issue at the other end of its pair (hidden), with
	// hidden's title, and own's parent ("" for none).
	type pair struct{ own, hidden, title, parent string }
	// overChild files a PUB parent and an open PRIV child under it.
	overChild := func(t *testing.T, name string) pair {
		parent := seed.issueUnder(t, seedPublic, name+", the PUB parent", "")
		title := name + ", the open PRIV child"
		return pair{own: parent, hidden: seed.issueUnder(t, seedPrivate, title, parent), title: title}
	}
	// underParent files a PRIV parent in triage and a PUB child under it.
	underParent := func(t *testing.T, name string) pair {
		title := name + ", the PRIV parent in triage"
		parent := seed.issueUnder(t, seedPrivate, title, "")
		return pair{own: seed.issueUnder(t, seedPublic, name+", the PUB child", parent), hidden: parent, title: title, parent: parent}
	}
	patch := func(status string) func(pair) (string, map[string]any) {
		return func(p pair) (string, map[string]any) {
			return "/api/v1/issues/" + p.own, map[string]any{"status": status}
		}
	}
	statusChange := func(body map[string]any) func(pair) (string, map[string]any) {
		return func(p pair) (string, map[string]any) { return "/api/v1/issues/" + p.own + "/status-change", body }
	}

	for _, row := range []struct {
		name   string
		family func(*testing.T, string) pair
		method string
		send   func(pair) (string, map[string]any)
		// status is own's status after alice's write, "" for a dry run, which writes nothing.
		status string
	}{
		{"a PATCH to done over an open PRIV child", overChild, http.MethodPatch, patch("done"), "done"},
		{"a status change to done over an open PRIV child", overChild, http.MethodPost,
			statusChange(map[string]any{"status": "done"}), "done"},
		{"the dry run of a status change to done over an open PRIV child", overChild, http.MethodPost,
			statusChange(map[string]any{"status": "done", "dry_run": true}), ""},
		{"a PATCH into work under a PRIV parent in triage", underParent, http.MethodPatch, patch("in_progress"), "in_progress"},
	} {
		t.Run(row.name, func(t *testing.T) {
			held := row.family(t, "carol's "+row.name)
			path, body := row.send(held)
			refused := seed.request(t, callerCarol, row.method, path, body)
			type listedIssue struct {
				Key string `json:"key"`
			}
			listed := decodeBody[struct {
				Code   string        `json:"code"`
				Issues []listedIssue `json:"issues"`
			}](t, refused)
			if refused.Code != http.StatusConflict || listed.Code != "PARENT_STATUS_CONFLICT" ||
				!slices.Contains(listed.Issues, listedIssue{Key: held.hidden}) {
				t.Fatalf("carol's write = %d %s, want 409 PARENT_STATUS_CONFLICT listing %s: the case proves nothing",
					refused.Code, refused.Body.String(), held.hidden)
			}

			hidden := row.family(t, "alice's "+row.name)
			ownBefore := seed.issueRow(t, hidden.own)
			hiddenStatus, hiddenParent, _ := seed.issueState(t, hidden.hidden)
			path, body = row.send(hidden)
			taken := seed.request(t, callerAlice, row.method, path, body)
			if taken.Code != http.StatusOK {
				t.Fatalf("alice's write = %d %s, want 200: an issue she does not see decided it", taken.Code, taken.Body.String())
			}
			// A PUB child's own parent field holds its PRIV parent's key, as GET shows it: the
			// answer holds that key there alone.
			text, shown := taken.Body.String(), 0
			if hidden.parent != "" {
				shown = strings.Count(text, `"parent":"`+hidden.hidden+`"`)
			}
			if strings.Contains(text, hidden.title) || strings.Count(text, hidden.hidden) > shown {
				t.Errorf("alice's write names the PRIV issue %s or its title: %s", hidden.hidden, text)
			}
			if status, parent, _ := seed.issueState(t, hidden.hidden); status != hiddenStatus || parent != hiddenParent {
				t.Errorf("alice's write moved the PRIV issue %s from %s under %q to %s under %q, want it as it was",
					hidden.hidden, hiddenStatus, hiddenParent, status, parent)
			}
			if row.status == "" {
				if after := seed.issueRow(t, hidden.own); after != ownBefore {
					t.Errorf("alice's dry run changed %s to %s, want nothing written", ownBefore, after)
				}
				return
			}
			rule.requireIssue(t, hidden.own, row.status, hidden.parent)
		})
	}
}

// issueState is the status, the parent ("" for none) and the last_seq of the issue key, as the
// database holds them.
func (s privateSeed) issueState(t *testing.T, key string) (status, parent string, lastSeq int) {
	t.Helper()
	var stored *string
	if err := s.database.Pool.QueryRow(t.Context(), `select status, parent_key, last_seq from issues where key = $1`, key).
		Scan(&status, &stored, &lastSeq); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return status, deref(stored), lastSeq
}

// issueRow is issueState as one string, which a write that leaves the issue as it was leaves
// equal.
func (s privateSeed) issueRow(t *testing.T, key string) string {
	t.Helper()
	status, parent, lastSeq := s.issueState(t, key)
	return key + " " + status + " " + parent + " " + strconv.Itoa(lastSeq)
}
