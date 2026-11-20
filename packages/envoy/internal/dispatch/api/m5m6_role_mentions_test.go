package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// roleTargets reads caller's GET target and answers every mention, delivery attempt and message
// target the answer carries, each as "<field> <target>", and the body.
func roleTargets(t *testing.T, seed hiddenHolderSeed, caller privateCaller, target string) ([]string, string) {
	t.Helper()
	body := seed.mustRequest(t, caller, http.MethodGet, target, nil, http.StatusOK).Body.String()
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("%s: decode GET %s: %v", caller, target, err)
	}
	var found []string
	var walk func(value any, field string)
	walk = func(value any, field string) {
		switch value := value.(type) {
		case map[string]any:
			for key, inner := range value {
				walk(inner, key)
			}
		case []any:
			for _, inner := range value {
				walk(inner, field)
			}
		case string:
			if field == "target" {
				found = append(found, "target "+value)
			}
		}
	}
	walk(decoded, "")
	return found, body
}

// TestM5M6ACommentNamesNoMentionOfAHiddenRole: GET /api/v1/issues/{key}/comments leaves out a
// mention, and a delivery attempt, whose target is a role of a private project its reader does not
// see, as the comment.created payload does: carol mentions role:legion-priv-architect, which
// session-pub holds, on PUB's issue (claimEventsWrite). carol's token that lists no project reads
// no target of that role; carol, who sees PRIV, reads the mention and its delivery.
//
// Control: the comment filter judging a target's session alone (holdsRoute's role arm dropped);
// carol's token then reads the role and the row fails naming it.
func TestM5M6ACommentNamesNoMentionOfAHiddenRole(t *testing.T) {
	seed := seedHiddenHolders(t, testServerOptions{})
	key, _ := seed.claimEventsWrite(t)
	target := "/api/v1/issues/" + key + "/comments"
	if found, body := roleTargets(t, seed, callerCarolToken, target); strings.Contains(body, privateProjectRole) {
		t.Errorf("carol's PUB token: GET %s answers the targets %v, naming role:%s, a role of PRIV: %s", target, found, privateProjectRole, body)
	}
	if found, _ := roleTargets(t, seed, callerCarol, target); countOf(found, "target role:"+privateProjectRole) < 2 {
		t.Errorf("carol: GET %s answers the targets %v, want the mention and its delivery to role:%s", target, found, privateProjectRole)
	}
}

// TestM5M6AMessageNamesNoTargetOfAHiddenRole: GET /api/v1/issues/{key}/messages answers a message
// whose target is a role of a private project its reader does not see with a null target, as the
// message.created payload does: carol messages PUB's issue to role:legion-priv-architect
// (claimEventsWrite). carol's token that lists no project reads no target of that role; carol, who
// sees PRIV, reads it.
//
// Control: the message filter judging a target's session alone (holdsRoute's role arm dropped);
// carol's token then reads the role and the row fails naming it.
func TestM5M6AMessageNamesNoTargetOfAHiddenRole(t *testing.T) {
	seed := seedHiddenHolders(t, testServerOptions{})
	key, _ := seed.claimEventsWrite(t)
	target := "/api/v1/issues/" + key + "/messages"
	if found, body := roleTargets(t, seed, callerCarolToken, target); strings.Contains(body, privateProjectRole) {
		t.Errorf("carol's PUB token: GET %s answers the targets %v, naming role:%s, a role of PRIV: %s", target, found, privateProjectRole, body)
	}
	if found, _ := roleTargets(t, seed, callerCarol, target); countOf(found, "target role:"+privateProjectRole) < 1 {
		t.Errorf("carol: GET %s answers the targets %v, want the message's target role:%s", target, found, privateProjectRole)
	}
}

// TestM5M6WithNoPrivateProjectCommentsAndMessagesReadAsBefore (R9): with PRIV public alice, a full
// member, reads the issue's comments and messages as the base answered them, the role named in
// both; each body is logged with its times and ids blanked, beside its sha256, for the report's
// comparison of the two commits.
//
// Control: the comment filter run for a reader that sees every object, with a role arm that hides
// every role - three lines: the filter's first test, seenReferences' first test and holdsRoute's
// role arm; alice then reads no role in the comments and the row fails.
func TestM5M6WithNoPrivateProjectCommentsAndMessagesReadAsBefore(t *testing.T) {
	seed := seedHiddenHolders(t, testServerOptions{})
	key, _ := seed.claimEventsWrite(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, target := range []string{"/api/v1/issues/" + key + "/comments", "/api/v1/issues/" + key + "/messages"} {
		found, body := roleTargets(t, seed, callerAlice, target)
		blanked := timestampsAndIDs.ReplaceAllString(body, "<varies>")
		t.Logf("R9 %s sha256 %x: %s", target, sha256.Sum256([]byte(blanked)), blanked)
		if countOf(found, "target role:"+privateProjectRole) == 0 {
			t.Errorf("PRIV public: alice's GET %s answers the targets %v, want role:%s among them, as before", target, found, privateProjectRole)
		}
	}
}

// countOf is how many of values equal value.
func countOf(values []string, value string) int {
	count := 0
	for _, each := range values {
		if each == value {
			count++
		}
	}
	return count
}
