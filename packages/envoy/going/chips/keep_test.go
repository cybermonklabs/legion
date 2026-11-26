package chips

import "testing"

// held is the Items of a test: each issue's status and title by its key, each document's name by
// its owner and slug, and each ask's, comment's and message's text by its kind, its issue and its
// id.
type held struct {
	issues    map[string][2]string
	documents map[[2]string]string
	texts     map[[3]string]string
}

func (h held) IssueChip(key string) (string, string, bool) {
	issue, found := h.issues[key]
	return issue[0], issue[1], found
}

func (h held) DocumentName(owner, slug string) (string, bool) {
	name, found := h.documents[[2]string{owner, slug}]
	return name, found
}

func (h held) ItemText(kind, issue, id string) (string, bool) {
	written, found := h.texts[[3]string{kind, issue, id}]
	return written, found
}

// The items the tests of KeepLinkWords link: OPS-4, a comment on it whose text the bar cuts to its
// first 60 characters, "p95 has stayed under 200 ms since the cache change landed on…", and a
// message on it whose text the bar draws whole.
const (
	keptComment = "dispatch://OPS-4/comment/6f1c2b9e-4d3a-4b8e-9c21-0a7d5e3f8b14"
	keptMessage = "dispatch://OPS-4/message/0b9e7c4d-2f6a-4e1b-8d35-7c4a9e2b1f60"
)

var keptItems = held{
	issues:    map[string][2]string{"OPS-4": {"in_progress", "Rotate the signing keys"}},
	documents: map[[2]string]string{{"OPS", "oncall-handbook"}: "On-call handbook"},
	texts: map[[3]string]string{
		{"comment", "OPS-4", "6f1c2b9e-4d3a-4b8e-9c21-0a7d5e3f8b14"}: "p95 has stayed under **200 ms** since the cache change landed on Monday, and the error rate is flat.",
		{"message", "OPS-4", "0b9e7c4d-2f6a-4e1b-8d35-7c4a9e2b1f60"}: "Pushed the rotation script; the staging run is green.",
	},
}

// TestALinksWordsThatSayMoreThanItsChipStandBesideIt: the bar draws the first link to an issue as
// the issue's chip in place of the link's words, and the eval's judge read every link to an item
// as the item's words, so the eval's answers lost what they wrote as a link's words: a session's
// name as the words of a link to its issue (c13's status-02 and status-05), and a comment's words,
// which the judge read as the comment's head, cut to its first 60 characters (status-06 and
// status-13). KeepLinkWords writes such words as text, and then the link, in parentheses, with the
// item's short form as its words and its address and title as written. A link whose words are the
// item's key, its title, the two together, its reference or a run of the head of its text says
// nothing the chip hides, and stays as written; so do a time, a web address, a picture, an
// autolink and a link in code. Each answer also holds a link that says more, so no case passes
// with no rewrite, and KeepLinkWords of what it wrote is what it wrote.
func TestALinksWordsThatSayMoreThanItsChipStandBesideIt(t *testing.T) {
	for _, c := range []struct{ name, answer, want string }{
		{
			"a session named as the words of a link to its issue",
			"[Implementer of OPS-4](dispatch://OPS-4) is unresponsive, and [Reviewer of OPS-4](/issues/OPS-4 \"the issue\") waits.",
			"Implementer of OPS-4 ([OPS-4](dispatch://OPS-4)) is unresponsive, and Reviewer of OPS-4 ([OPS-4](/issues/OPS-4 \"the issue\")) waits.",
		},
		{
			"a comment's words past the head its chip draws",
			"bob@example.com wrote [p95 has stayed under 200 ms since the cache change landed on Monday](" + keptComment + ").",
			"bob@example.com wrote p95 has stayed under 200 ms since the cache change landed on Monday ([OPS-4 comment](" + keptComment + ")).",
		},
		{
			"a message's words that its chip does not draw",
			"[Implementer of OPS-4 says the staging run is green](" + keptMessage + ").",
			"Implementer of OPS-4 says the staging run is green ([OPS-4 message](" + keptMessage + ")).",
		},
		{
			"a document's name with more words",
			"See [the paging section of the on-call handbook](dispatch://OPS/artifact/oncall-handbook).",
			"See the paging section of the on-call handbook ([OPS/oncall-handbook](dispatch://OPS/artifact/oncall-handbook)).",
		},
		{
			"words that are the key, the title, both, the reference or the head",
			"[OPS-4](dispatch://OPS-4), [rotate the signing keys](/issues/OPS-4), [OPS-4: Rotate the signing keys](dispatch://OPS-4), " +
				"[Rotate the signing keys (OPS-4)](dispatch://OPS-4), [OPS-4 · In progress](dispatch://OPS-4), [OPS-4 comment](" + keptComment + "), " +
				"[p95 has stayed under 200 ms](" + keptComment + "), [the staging run is green](" + keptMessage + ") and " +
				"[On-call handbook](dispatch://OPS/artifact/oncall-handbook) stay beside [Reviewer of OPS-4](dispatch://OPS-4).",
			"[OPS-4](dispatch://OPS-4), [rotate the signing keys](/issues/OPS-4), [OPS-4: Rotate the signing keys](dispatch://OPS-4), " +
				"[Rotate the signing keys (OPS-4)](dispatch://OPS-4), [OPS-4 · In progress](dispatch://OPS-4), [OPS-4 comment](" + keptComment + "), " +
				"[p95 has stayed under 200 ms](" + keptComment + "), [the staging run is green](" + keptMessage + ") and " +
				"[On-call handbook](dispatch://OPS/artifact/oncall-handbook) stay beside Reviewer of OPS-4 ([OPS-4](dispatch://OPS-4)).",
		},
		{
			"a time, a web address, a picture, an autolink and code",
			"[5 days ago](dispatch-time:2026-11-18T11:35:00Z), [the runbook](https://dispatch.example/issues/OPS-4), " +
				"![the graph](dispatch://OPS-4), <dispatch://OPS-4>, [the component](dispatch://OPS/OPS-3), " +
				"`[Implementer of OPS-4](dispatch://OPS-4)` and\n```\n[Implementer of OPS-4](dispatch://OPS-4)\n```\n" +
				"stay beside [Implementer of OPS-4](dispatch://OPS-4).",
			"[5 days ago](dispatch-time:2026-11-18T11:35:00Z), [the runbook](https://dispatch.example/issues/OPS-4), " +
				"![the graph](dispatch://OPS-4), <dispatch://OPS-4>, [the component](dispatch://OPS/OPS-3), " +
				"`[Implementer of OPS-4](dispatch://OPS-4)` and\n```\n[Implementer of OPS-4](dispatch://OPS-4)\n```\n" +
				"stay beside Implementer of OPS-4 ([OPS-4](dispatch://OPS-4)).",
		},
		{
			"an issue the store does not hold",
			"[Rotate the signing keys](dispatch://OPS-5) and [OPS-5](dispatch://OPS-5).",
			"Rotate the signing keys ([OPS-5](dispatch://OPS-5)) and [OPS-5](dispatch://OPS-5).",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := KeepLinkWords(c.answer, keptItems)
			if got != c.want {
				t.Fatalf("KeepLinkWords(%q)\n= %q\nwant %q", c.answer, got, c.want)
			}
			if again := KeepLinkWords(got, keptItems); again != got {
				t.Errorf("KeepLinkWords of what it wrote, %q, is %q", got, again)
			}
		})
	}
}
