package eval

import (
	"testing"

	"github.com/sjawhar/envoy/going/chips"
)

// TestAnAnswerIsRenderedAsTheBarDrawsItsLinks: on the seed, each link the bar draws as an item is
// written as the bar draws it. A link to a done issue, by its reference or by its dashboard path,
// is its chip: its key, "Done" and its title. A link to a key the store does not hold is the bare
// key, and one to a document it does not hold the bare reference. A link to a document or a file
// is its name, one to a spec the issue's title, one to an ask the head of its question, and one to
// a comment the head of its text with its reference in short form. A link the bar draws as no
// item, a picture, and a link in code stay as written.
func TestAnAnswerIsRenderedAsTheBarDrawsItsLinks(t *testing.T) {
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, answer, want string }{
		{
			"a done issue by its reference",
			"[DEMO-11](dispatch://DEMO-11) is closed.",
			"[DEMO-11 · Done · Board: card titles cut off at two lines] is closed.",
		},
		{
			"a done issue by its dashboard path, a title and an autolink",
			"See [the card titles](/issues/DEMO-11 \"card titles\") and <dispatch://DEMO-12>.",
			"See [DEMO-11 · Done · Board: card titles cut off at two lines] and [DEMO-12 · In progress · Export: CSV and PDF].",
		},
		{
			"an unknown key and an unknown document",
			"Filed as [DEMO-404, done](dispatch://DEMO-404); the log is [the log](/issues/DEMO-16/artifacts/no-such-log).",
			"Filed as [DEMO-404]; the log is [DEMO-16/artifact/no-such-log].",
		},
		{
			"a document, a file and a spec",
			"Read [the handbook](/projects/OPS/documents/oncall-handbook), [the error](dispatch://DEMO-23/artifact/upload-error-txt) and [its spec](dispatch://DEMO-3/spec).",
			"Read [On-call handbook], [upload-error.txt] and [Back-links in issue specs].",
		},
		{
			"an ask and a comment",
			"[the ask](/issues/DEMO-3/asks/" + ItemID("ask", "a01") + ") and [the note](dispatch://DEMO-16/comment/" + ItemID("comment", "relates-DEMO-16-DEMO-22") + ")",
			"[Should the back-link come first in the spec, above the…] and [Related to DEMO-22.]",
		},
		{
			"links the bar draws as no item",
			"[OPS-3](dispatch://OPS/OPS-3), [DEMO-1](https://dispatch.example/issues/DEMO-1), ![shot](dispatch://DEMO-11) and dispatch://DEMO-11.",
			"[OPS-3](dispatch://OPS/OPS-3), [DEMO-1](https://dispatch.example/issues/DEMO-1), ![shot](dispatch://DEMO-11) and dispatch://DEMO-11.",
		},
		{
			"links in code",
			"`[DEMO-11](dispatch://DEMO-11)`\n\n```\n[DEMO-11](dispatch://DEMO-11)\n```\n[DEMO-11](dispatch://DEMO-11)",
			"`[DEMO-11](dispatch://DEMO-11)`\n\n```\n[DEMO-11](dispatch://DEMO-11)\n```\n[DEMO-11 · Done · Board: card titles cut off at two lines]",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderAnswer(c.answer, seed); got != c.want {
				t.Errorf("RenderAnswer(%q)\n= %q\nwant %q", c.answer, got, c.want)
			}
		})
	}
}

// TestATimeLinkIsRenderedWithTheDateItsHoverShows: the agent writes a time as a link whose words
// are the time as the page writes it and whose address is the instant, under dispatch-time:, and
// the bar shows the words with the instant's full date on hover (the web's dispatch-time.tsx). The
// judge reads the words and then that date in UTC, its seconds where it has some, so a fact that
// names the time holds only where the link's instant is right. An instant written with an offset
// or to the minute is read as the web reads it, and the scheme in any case with spaces around the
// address. An address that names no instant - no time's shape, a date alone, a day no month has -
// gives the words alone, as the bar draws them; a time link in code stays as written.
func TestATimeLinkIsRenderedWithTheDateItsHoverShows(t *testing.T) {
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, answer, want string }{
		{
			"a time in relative words beside a chip",
			"[DEMO-11](dispatch://DEMO-11) closed [5 days ago](dispatch-time:2026-11-18T11:35:00Z).",
			"[DEMO-11 · Done · Board: card titles cut off at two lines] closed 5 days ago (18 Nov 2026, 11:35 UTC).",
		},
		{
			"an exact time with seconds and an offset",
			"It started [at 08:59:01 UTC on 15 Nov 2026](dispatch-time:2026-11-15T09:59:01.250+01:00).",
			"It started at 08:59:01 UTC on 15 Nov 2026 (15 Nov 2026, 08:59:01 UTC).",
		},
		{
			"a time to the minute, its scheme in capitals, and an autolink",
			"Seen [yesterday]( DISPATCH-TIME:2026-11-17T09:40-05:00 ) and <dispatch-time:2026-11-18T11:35:00Z>.",
			"Seen yesterday (17 Nov 2026, 14:40 UTC) and dispatch-time:2026-11-18T11:35:00Z (18 Nov 2026, 11:35 UTC).",
		},
		{
			"addresses that name no instant",
			"[soon](dispatch-time:tomorrow), [that day](dispatch-time:2026-11-18), [never](dispatch-time:2026-11-31T09:00:00Z) and [then](dispatch-time:).",
			"soon, that day, never and then.",
		},
		{
			"a time link in code",
			"`[5 days ago](dispatch-time:2026-11-18T11:35:00Z)`",
			"`[5 days ago](dispatch-time:2026-11-18T11:35:00Z)`",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderAnswer(c.answer, seed); got != c.want {
				t.Errorf("RenderAnswer(%q)\n= %q\nwant %q", c.answer, got, c.want)
			}
		})
	}
}

// TestATimeLinkWhoseWordsAreAKeyIsRenderedAsTheChipAndTheDate: the agent writes when an issue
// last moved as a time link whose words are the issue's key, [DEMO-12](dispatch-time:...), and
// the bar draws those words as the issue's chip, its key, status and title, with the time on hover
// (the web's timeLinkIssue in MarkdownBody's timeLinkAnchors). The judge reads the chip's words, as
// it reads a link to the issue, and then the date: the bare key for an issue the store does not
// hold, and the chip alone for an address that names no instant. Words around the key with spaces
// are the key; words that are more than a key, or a key the web reads as none, stay words.
func TestATimeLinkWhoseWordsAreAKeyIsRenderedAsTheChipAndTheDate(t *testing.T) {
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, answer, want string }{
		{
			"the key of an issue the seed holds",
			"[DEMO-12](dispatch-time:2026-10-10T09:00:00Z) moved last, and [ DEMO-11 ](dispatch-time:2026-11-15T09:59:01+01:00) closed.",
			"[DEMO-12 · In progress · Export: CSV and PDF] (10 Oct 2026, 09:00 UTC) moved last, and [DEMO-11 · Done · Board: card titles cut off at two lines] (15 Nov 2026, 08:59:01 UTC) closed.",
		},
		{
			"the key of an issue the seed does not hold",
			"[DEMO-404](dispatch-time:2026-10-10T09:00:00Z) moved last.",
			"[DEMO-404] (10 Oct 2026, 09:00 UTC) moved last.",
		},
		{
			"a key whose address names no instant",
			"[DEMO-12](dispatch-time:tomorrow) moved last.",
			"[DEMO-12 · In progress · Export: CSV and PDF] moved last.",
		},
		{
			"a key the page reads through its marks",
			"[**DEMO-12**](dispatch-time:2026-10-10T09:00:00Z) and [DEMO\\-11](dispatch-time:2026-11-15T09:59:01+01:00) moved, [*DEMO-12 moved*](dispatch-time:2026-10-10T09:00:00Z) not.",
			"[DEMO-12 · In progress · Export: CSV and PDF] (10 Oct 2026, 09:00 UTC) and [DEMO-11 · Done · Board: card titles cut off at two lines] (15 Nov 2026, 08:59:01 UTC) moved, *DEMO-12 moved* (10 Oct 2026, 09:00 UTC) not.",
		},
		{
			"words that are not a key alone",
			"[DEMO-12 moved](dispatch-time:2026-10-10T09:00:00Z), [demo-12](dispatch-time:2026-10-10T09:00:00Z), [DEMO-12/plan](dispatch-time:2026-10-10T09:00:00Z) and [OPS](dispatch-time:2026-10-10T09:00:00Z).",
			"DEMO-12 moved (10 Oct 2026, 09:00 UTC), demo-12 (10 Oct 2026, 09:00 UTC), DEMO-12/plan (10 Oct 2026, 09:00 UTC) and OPS (10 Oct 2026, 09:00 UTC).",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderAnswer(c.answer, seed); got != c.want {
				t.Errorf("RenderAnswer(%q)\n= %q\nwant %q", c.answer, got, c.want)
			}
		})
	}
}

// TestAnAnswerWhoseLinksKeepTheirWordsRendersThem: the server stores an answer with the words of
// each link that say more than its chip draws written beside the chip (chips.KeepLinkWords), so
// the judge reads them where it read the chip alone (c13 at 9ae5605b): a session's name written
// as the words of a link to its issue, a comment's words past the head the bar cuts its text to,
// and words a message's head does not hold. A link whose words are the item's key stays a chip.
func TestAnAnswerWhoseLinksKeepTheirWordsRendersThem(t *testing.T) {
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	answer := "[Reviewer of DEMO-22](dispatch://DEMO-22) is reviewing [DEMO-22](dispatch://DEMO-22); " +
		"[chen@example.com wants to split this epic into CSV export and PDF export](dispatch://DEMO-12/comment/" + ItemID("comment", "c01") + "), " +
		"and [the planner waits on the ask about saving the order](dispatch://DEMO-7/message/" + ItemID("message", "m01") + ")."
	want := "Reviewer of DEMO-22 ([DEMO-22 · Needs review · Export: progress bar for large exports]) is reviewing " +
		"[DEMO-22 · Needs review · Export: progress bar for large exports]; " +
		"chen@example.com wants to split this epic into CSV export and PDF export ([The CSV and PDF halves share nothing but the export menu. I…]), " +
		"and the planner waits on the ask about saving the order ([Plan draft: 1) a drag handle on each column header, 2) save…])."
	if got := RenderAnswer(chips.KeepLinkWords(answer, seed), seed); got != want {
		t.Errorf("RenderAnswer of the stored answer\n= %q\nwant %q", got, want)
	}
}

// TestATrysStoreRendersAnAnswerAsTheSeedDoes: the seeded store, as a try's grade reads it, renders
// every calibration answer of labels.json as the seed does, and a store renders an issue it holds
// in the status it holds.
func TestATrysStoreRendersAnAnswerAsTheSeedDoes(t *testing.T) {
	_, seed, tasks, before := seededStore(t)
	labels, err := LoadLabels(Files, tasks)
	if err != nil {
		t.Fatal(err)
	}
	for i, answer := range labels.Answers {
		if fromStore, fromSeed := RenderAnswer(answer.Answer, before), RenderAnswer(answer.Answer, seed); fromStore != fromSeed {
			t.Errorf("answer %d (%s) renders on the store as\n%q\nand on the seed as\n%q", i+1, answer.Task, fromStore, fromSeed)
		}
	}
	issue := before.Issues["DEMO-12"]
	issue.Status = "needs_review"
	before.Issues["DEMO-12"] = issue
	if got, want := RenderAnswer("[DEMO-12](/issues/DEMO-12)", before), "[DEMO-12 · Needs review · Export: CSV and PDF]"; got != want {
		t.Errorf("the store renders DEMO-12 as %q, want %q", got, want)
	}
}
