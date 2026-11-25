package record

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The merger's role prompt (prompts/roles/merger.md, #4979's line, kept as main has it) says what a
// merge-forward message gives the merger: "the daemon's merge-forward message on your role topic
// says that two reads found the head conflicting", "When a merge-forward message says instead that
// the implementer is asked to merge the base forward ... wait for the message that names the new
// head". Indirect speech names facts, not words (the architect's ruling, round 17): each fact that
// line says a merge-forward message gives is a field of the record the merger receives, as main's
// messages gave it ("conflicts with its base branch %s, and a second read of GitHub confirmed it";
// "The implementer is asked to merge %s forward"; "the new head is %s"). texts.Entries lists the
// merger among the readers of merge-forward-ask, merge-forward-hold and merge-forward-landed.
func TestEachFactTheMergersLineSaysAMergeForwardMessageGivesIsAField(t *testing.T) {
	head, landed := strings.Repeat("ab", 20), strings.Repeat("cd", 20)
	for _, c := range []struct {
		name    string
		message MergeForwardMessage
		key     string
		want    string
	}{
		{name: "the merger's ask in merging: two reads found the head conflicting",
			message: MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardAsk, Head: head, Base: "main", Phase: phase.Merging,
				PullRequest: 1501, Why: ConflictMergeNext},
			key: "conflict_reads", want: "2"},
		{name: "the merger's hold behind the implementer's ask: the implementer is asked",
			message: MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardHold, Head: head, Base: "main", Phase: phase.Merging,
				PullRequest: 1501, Why: ConflictCheckNotGreen},
			key: "asked", want: string(claim.RoleImplementer)},
		{name: "the merger's landed in merging: the new head",
			message: MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardLanded, Head: head, Landed: landed, Base: "main",
				Phase: phase.Merging, PullRequest: 1501},
			key: "landed", want: landed},
	} {
		t.Run(c.name, func(t *testing.T) {
			if entry, ok := texts.Lookup(c.message.Kind()); !ok || !strings.Contains(roleList(entry.Readers), string(claim.RoleMerger)) {
				t.Fatalf("the list names %v as the readers of %s, want the merger among them", entry.Readers, c.message.Kind())
			}
			published := c.message.Record("LEGION-41", PullRequest{Repo: "acme/tower"})
			encoded, err := json.Marshal(published)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("the %s the merger receives: %s", c.message.Kind(), encoded)
			if got := fieldsOfPublished(t, published)[c.key]; got != c.want {
				t.Errorf("the %s holds %s = %q, want %q: merger.md says the message gives that fact", c.message.Kind(), c.key, got, c.want)
			}
		})
	}
}

// roleList is roles joined by commas.
func roleList(roles []claim.Role) string {
	words := make([]string, len(roles))
	for i, role := range roles {
		words[i] = string(role)
	}
	return strings.Join(words, ",")
}
