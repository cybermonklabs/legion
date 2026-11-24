package plan

import (
	"regexp"
	"strings"
	"testing"
)

// A task's slug, which goes into its branch and its directory, is the task's id when the id is a
// short lower-case word, and otherwise a name made from a hash of the id: an id of any other shape,
// a capital letter, a slash, two dots or 41 characters, would make no valid branch or no stable
// directory, and neither would a word no slug may be (claim's notSlugs), `lock`, `removing` and
// `join`.
func TestASlugIsTheIdOrAHashOfIt(t *testing.T) {
	if got := TaskSlug("d"); got != "d" {
		t.Fatalf("TaskSlug(d) = %q", got)
	}
	uuid := "3f2c1a9e-6b7d-4e1f-9a2b-0c4d5e6f7a8b"
	if got := TaskSlug(uuid); got != uuid {
		t.Fatalf("a lower-case UUID is a slug: got %q", got)
	}
	for _, id := range []string{"D", "a/b", "a..b", strings.Repeat("a", 41), "", "lock", "removing", "join"} {
		got := TaskSlug(id)
		if !regexp.MustCompile(`^t-[0-9a-f]{12}$`).MatchString(got) {
			t.Errorf("TaskSlug(%q) = %q, want t- and 12 hex", id, got)
		}
	}
	if TaskSlug("D") == TaskSlug("E") {
		t.Fatal("two ids gave one slug")
	}
}
