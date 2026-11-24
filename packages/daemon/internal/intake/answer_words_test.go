package intake

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// TestAnOpenedReadsEventIDNamesNoCommitThatIsNotOne: the opened a read of GitHub applies goes under
// an event id that names the head GitHub lists, which a log line can print (ApplyFact's errors).
// Where GitHub, or whatever answers in its place, lists a head that is no sha, echoing the token
// the read carried and quoting a URL carrying made-up credentials, the id names it by a digest,
// which still tells it from another such head, so the opened is applied once per head as it came
// (echo). The control's sha is named as it came. Captures intake.answer-words/opened-read-<case>.
func TestAnOpenedReadsEventIDNamesNoCommitThatIsNotOne(t *testing.T) {
	const token = "outbox-token"
	read := func(head string) string {
		var pull githubrest.PullRequest
		pull.Number, pull.Head.SHA = 86, head
		_, id := OpenedRead("CAPTURE", "acme/widgets", "legion/CAPTURE-1", pull, 1, nil)
		return id
	}
	echo := read("token " + token + " refused; " + oauth2calltest.Words)
	oauth2calltest.RequireNoPiece(t, "intake.answer-words/opened-read-echo", echo, token)
	if echo == read("token "+token+" refused again; "+oauth2calltest.Words) {
		t.Errorf("two heads that are no sha share the event id %q", echo)
	}
	const commit = "0123456789abcdef0123456789abcdef01234567"
	control := read(commit)
	oauth2calltest.RequireNone(t, "intake.answer-words/opened-read-control", control)
	if !strings.Contains(control, ":"+commit+":") {
		t.Errorf("the opened's event id is %q, want it to name %s", control, commit)
	}
}
