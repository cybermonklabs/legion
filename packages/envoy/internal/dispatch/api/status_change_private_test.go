package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// statusChangeAnswerOf is a status change's 200 answer as these tests read it: the issue, and the
// keys and issues of the entries it lists as changed.
func statusChangeAnswerOf(t *testing.T, text string) statusChangeAnswer {
	t.Helper()
	var answer statusChangeAnswer
	if err := json.Unmarshal([]byte(text), &answer); err != nil {
		t.Fatalf("decode the status change's answer %s: %v", text, err)
	}
	return answer
}

// changedKeys is the keys of the entries answer lists as changed, in its order.
func changedKeys(answer statusChangeAnswer) []string {
	keys := []string{}
	for _, entry := range answer.Changed {
		keys = append(keys, entry.Key)
	}
	return keys
}

// TestAStatusChangeMovesAndNamesNoIssueItsCallerDoesNotSee (the live reviewer's F12): a status
// change reads its cascade as its caller reads it (readStatusChange). alice, a full member PRIV's
// list does not name, sees PUB alone, and her credential names no project, so no project check
// leaves anything out for her. Each call below, and its dry run, of a PUB issue whose cascade
// reaches a PRIV issue, moves and unlinks neither that issue nor any issue the cascade reaches
// only through it, a PUB one included, and its answer lists only the PUB issue it names and holds
// neither the other issues' keys nor their titles:
//
//   - include_children into work over a PRIV child, which has a PUB child of its own;
//   - include_parent into work under a PRIV parent, which has a PUB parent of its own; the PUB
//     issue's own parent field holds the PRIV parent's key, as GET shows it;
//   - unlink_children over a PRIV child.
//
// carol, on PRIV's list, makes the same call on a family of her own and moves or unlinks the PRIV
// issue and the PUB one beyond it, so each family reaches them.
//
// Control: hiddenFromStatusChange judging what the reader sees only for a credential that names a
// project (`callerProject != "" &&` before its seenReferences check), under which alice's cascade
// is taken whole: her dry runs list the PRIV issue and the issue beyond it (the "lists" check
// fails), and her calls stop at the PRIV issue's step, 404.
func TestAStatusChangeMovesAndNamesNoIssueItsCallerDoesNotSee(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	// family is the PUB issue a call names (named), the PRIV issue its cascade reaches first
	// (hidden), and the PUB issue the cascade reaches only through hidden (beyond), with the
	// titles of the two.
	type family struct{ named, hidden, beyond, hiddenTitle, beyondTitle string }
	below := func(t *testing.T, name string) family {
		f := family{hiddenTitle: name + ", the PRIV child", beyondTitle: name + ", the PUB grandchild"}
		f.named = seed.issueUnder(t, seedPublic, name+", the PUB parent", "")
		f.hidden = seed.issueUnder(t, seedPrivate, f.hiddenTitle, f.named)
		f.beyond = seed.issueUnder(t, seedPublic, f.beyondTitle, f.hidden)
		return f
	}
	above := func(t *testing.T, name string) family {
		f := family{hiddenTitle: name + ", the PRIV parent", beyondTitle: name + ", the PUB grandparent"}
		f.beyond = seed.issueUnder(t, seedPublic, f.beyondTitle, "")
		f.hidden = seed.issueUnder(t, seedPrivate, f.hiddenTitle, f.beyond)
		f.named = seed.issueUnder(t, seedPublic, name+", the PUB child", f.hidden)
		return f
	}
	alone := func(t *testing.T, name string) family {
		f := family{hiddenTitle: name + ", the PRIV child"}
		f.named = seed.issueUnder(t, seedPublic, name+", the PUB parent", "")
		f.hidden = seed.issueUnder(t, seedPrivate, f.hiddenTitle, f.named)
		return f
	}
	for _, row := range []struct {
		name   string
		family func(*testing.T, string) family
		body   map[string]any
		// named is the named issue's status after the call; moved is the hidden issue's and the
		// beyond issue's status and parent after carol's call.
		named string
		moved func(f family) [4]string
	}{
		{"include_children into work over a PRIV child", below, map[string]any{"status": "todo", "include_children": true}, "todo",
			func(f family) [4]string { return [4]string{"todo", f.named, "todo", f.hidden} }},
		{"include_parent into work under a PRIV parent", above, map[string]any{"status": "in_progress", "include_parent": true}, "in_progress",
			func(f family) [4]string { return [4]string{"in_progress", f.beyond, "in_progress", ""} }},
		{"unlink_children over a PRIV child", alone, map[string]any{"status": "backlog", "unlink_children": true}, "backlog",
			func(f family) [4]string { return [4]string{model.IssueStatusTriage, ""} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			proved := row.family(t, "carol's call "+row.name)
			moved := seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+proved.named+"/status-change", row.body, http.StatusOK)
			if keys := changedKeys(statusChangeAnswerOf(t, moved.Body.String())); !slices.Contains(keys, proved.hidden) {
				t.Fatalf("carol's call lists %v as changed, want %s among them: the case proves nothing", keys, proved.hidden)
			}
			want := row.moved(proved)
			if status, parent, _ := seed.issueState(t, proved.hidden); status != want[0] || parent != want[1] {
				t.Fatalf("carol's call left %s %s under %q, want %s under %q: the case proves nothing", proved.hidden, status, parent, want[0], want[1])
			}
			if proved.beyond != "" {
				if status, parent, _ := seed.issueState(t, proved.beyond); status != want[2] || parent != want[3] {
					t.Fatalf("carol's call left %s %s under %q, want %s under %q: the case proves nothing", proved.beyond, status, parent, want[2], want[3])
				}
			}

			f := row.family(t, "alice's call "+row.name)
			path := "/api/v1/issues/" + f.named + "/status-change"
			// The issues left out keep their status and parent. A PRIV parent hears the named
			// issue's child.status event, as the parent of every child whose status moves does, so
			// its last_seq is not what stays.
			kept := map[string][2]string{}
			for _, key := range []string{f.hidden, f.beyond} {
				if key != "" {
					status, parent, _ := seed.issueState(t, key)
					kept[key] = [2]string{status, parent}
				}
			}
			// leftOut fails t unless text, an answer of alice's, lists f.named alone and holds no
			// key but the named issue's own parent field's, and no title, of the issues left out.
			leftOut := func(t *testing.T, what, text string) {
				t.Helper()
				if keys := changedKeys(statusChangeAnswerOf(t, text)); !slices.Equal(keys, []string{f.named}) {
					t.Errorf("%s lists %v as changed, want %s alone: %s", what, keys, f.named, text)
				}
				shown := strings.Count(text, `"parent":"`+f.hidden+`"`)
				if strings.Count(text, f.hidden) > shown || strings.Contains(text, f.hiddenTitle) ||
					(f.beyond != "" && (strings.Contains(text, f.beyond) || strings.Contains(text, f.beyondTitle))) {
					t.Errorf("%s names an issue the cascade leaves out: %s", what, text)
				}
			}
			planned := seed.request(t, callerAlice, http.MethodPost, path, withMembers(row.body, map[string]any{"dry_run": true}))
			if planned.Code != http.StatusOK {
				t.Fatalf("alice's dry run = %d %s, want 200", planned.Code, planned.Body.String())
			}
			leftOut(t, "alice's dry run", planned.Body.String())
			called := seed.request(t, callerAlice, http.MethodPost, path, row.body)
			if called.Code != http.StatusOK {
				t.Fatalf("alice's call = %d %s, want 200", called.Code, called.Body.String())
			}
			leftOut(t, "alice's call", called.Body.String())
			for key, before := range kept {
				if status, parent, _ := seed.issueState(t, key); [2]string{status, parent} != before {
					t.Errorf("alice's call moved %s from %v to %s under %q, want it as it was", key, before, status, parent)
				}
			}
			if status, _, _ := seed.issueState(t, f.named); status != row.named {
				t.Errorf("alice's call left %s %s, want %s", f.named, status, row.named)
			}
		})
	}
}

// TestAStatusChangeAnswersItsIssuesAsItsCallerReadsThem (the live reviewer's F12): a status
// change answers the issue it names, and each issue it changed, as the PATCH's route answers its
// issue (leaveOutHiddenIssueReferences), and a keyed call keeps them so in its recovery point,
// which a repeat is answered from. carol files a PUB issue blocked by PRIV's issue. alice sees PUB
// alone, and GET answers her the PUB issue with no blocker. Her dry run, her call and a keyed call
// of a twin issue answer that issue, and each entry's issue, with no blocker and without PRIV's
// key; her repeat of the keyed call is replayed, the first answer's bytes again. carol's call of a
// third twin answers PRIV's key among its blockers, so each issue holds it.
//
// Controls: the dry run answered without leaveOutHiddenIssueReferences (changeIssueStatus), under
// which the dry run names PRIV's key; and writeIssue's step answering after rather than shown
// (the leaveOutHiddenIssueReferences of a step taken out), under which the call, the keyed call
// and the keyed call's stored point name it, so alice's repeat is refused 409 KEY_COMMITTED, since
// pointSeen judges PRIV's blocker (the "replayed" check fails).
func TestAStatusChangeAnswersItsIssuesAsItsCallerReadsThem(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	blocker := seed.issue[seedPrivate]
	blocked := func(t *testing.T, name string) string {
		key := seed.issueUnder(t, seedPublic, name, "")
		seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"blocked_by": []string{blocker}}, http.StatusOK)
		return key
	}
	proved := blocked(t, "Blocked by PRIV, moved by carol")
	moved := seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+proved+"/status-change", map[string]any{"status": "todo"}, http.StatusOK)
	if answer := statusChangeAnswerOf(t, moved.Body.String()); !slices.Contains(answer.Issue.BlockedBy, blocker) {
		t.Fatalf("carol's call answers %s blocked by %v, want %s among them: the case proves nothing", proved, answer.Issue.BlockedBy, blocker)
	}

	// shown fails t unless text, an answer of alice's, holds the issue and each entry's issue
	// with no blocker, and PRIV's key nowhere.
	shown := func(t *testing.T, what, text string) {
		t.Helper()
		answer := statusChangeAnswerOf(t, text)
		if len(answer.Issue.BlockedBy) != 0 || strings.Contains(text, blocker) {
			t.Errorf("%s names PRIV's issue %s: %s", what, blocker, text)
		}
		for _, entry := range answer.Changed {
			if entry.Issue != nil && len(entry.Issue.BlockedBy) != 0 {
				t.Errorf("%s answers the entry of %s blocked by %v: %s", what, entry.Key, entry.Issue.BlockedBy, text)
			}
		}
	}
	read := blocked(t, "Blocked by PRIV, moved by alice")
	if issue := decodeBody[model.Issue](t, seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/"+read, nil, http.StatusOK)); len(issue.BlockedBy) != 0 {
		t.Fatalf("alice's GET answers %s blocked by %v, want none: the comparison proves nothing", read, issue.BlockedBy)
	}
	path := "/api/v1/issues/" + read + "/status-change"
	planned := seed.mustRequest(t, callerAlice, http.MethodPost, path, map[string]any{"status": "todo", "dry_run": true}, http.StatusOK)
	shown(t, "alice's dry run", planned.Body.String())
	called := seed.mustRequest(t, callerAlice, http.MethodPost, path, map[string]any{"status": "todo"}, http.StatusOK)
	shown(t, "alice's call", called.Body.String())

	keyed := blocked(t, "Blocked by PRIV, moved by alice under a key")
	keyedPath := "/api/v1/issues/" + keyed + "/status-change"
	body := map[string]any{"status": "todo"}
	first := keyedSeedRequest(t, seed, callerAlice, http.MethodPost, keyedPath, body, "alice-moves-a-blocked-issue")
	if first.Code != http.StatusOK {
		t.Fatalf("alice's keyed call = %d %s, want 200", first.Code, first.Body.String())
	}
	shown(t, "alice's keyed call", first.Body.String())
	again := keyedSeedRequest(t, seed, callerAlice, http.MethodPost, keyedPath, body, "alice-moves-a-blocked-issue")
	if again.Code != http.StatusOK || again.Header().Get(contracts.IdempotencyReplayedHeader) == "" {
		t.Fatalf("alice's repeat = %d %s (replayed %q), want the first answer replayed", again.Code, again.Body.String(),
			again.Header().Get(contracts.IdempotencyReplayedHeader))
	}
	shown(t, "alice's repeat", again.Body.String())
	if again.Body.String() != first.Body.String() {
		t.Errorf("alice's repeat answers %s, want the first answer %s", again.Body.String(), first.Body.String())
	}
}
