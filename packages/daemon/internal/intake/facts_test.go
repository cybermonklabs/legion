package intake

import "testing"

// Every Legion role appends a footer to a pull-request review it submits, naming the session that
// wrote it (skills/legion-worker/SKILL.md, prompts/roles/reviewer.md). LegionSession finds the
// last one, since a review may quote an earlier footer (a reply, a reviewer's own draft) before
// appending its own: workflow's byReviewer tells the reviewer's own review-App session from any
// other's by it.
func TestLegionSessionFindsTheLastFooterInAReviewsBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "a footer at the end, after a multi-line body",
			body: "Looks good.\n\nA few nits, but nothing blocking.\n\n<!-- legion: {\"session\":\"01a11940-9d2f-723c-b7de-d65a2e3ce427\",\"phase\":\"review\"} -->",
			want: "01a11940-9d2f-723c-b7de-d65a2e3ce427",
		},
		{name: "no footer", body: "Looks good.", want: ""},
		{
			name: "two footers; the last one wins",
			body: "<!-- legion: {\"session\":\"earlier\",\"phase\":\"review\"} -->\n\nActually, one more thing.\n\n<!-- legion: {\"session\":\"later\",\"phase\":\"review\"} -->",
			want: "later",
		},
		{name: "malformed JSON", body: "<!-- legion: {not json} -->", want: ""},
		{
			name: "a CRLF body",
			body: "Looks good.\r\n\r\n<!-- legion: {\"session\":\"crlf-session\",\"phase\":\"review\"} -->",
			want: "crlf-session",
		},
		{
			name: "the footer is not the body's last text; the last occurrence still wins",
			body: "<!-- legion: {\"session\":\"the-session\",\"phase\":\"review\"} -->\n\nedit: fixed a typo above.",
			want: "the-session",
		},
		{name: "an empty session field", body: "<!-- legion: {\"session\":\"\",\"phase\":\"review\"} -->", want: ""},
		{name: "the body is exactly the footer", body: "<!-- legion: {\"session\":\"only-thing-here\",\"phase\":\"review\"} -->", want: "only-thing-here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := PullRequestReview{Body: tc.body}
			if got := review.LegionSession(); got != tc.want {
				t.Fatalf("LegionSession() of %q = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// The same footer names the head the review read, which is the only word a review has for the
// code it saw: GitHub moves a review's commit_id to the pull request's current head
// (dispatch://INFRA-1557), so the workflow holds a review-App review to its footer's head
// instead (workflow's refusesRound). Only a whole commit name counts - 40 lowercase hexadecimal
// characters - since a prefix or a branch name names no commit to compare with the head.
func TestLegionHeadReadsTheHeadTheReviewSaysItRead(t *testing.T) {
	const head = "8848896da05779c592221b1199ee55ddb11cc3f3"
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "the head the review read",
			body: "Round 2: APPROVE.\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\",\"head\":\"" + head + "\"} -->",
			want: head,
		},
		{
			name: "a short head names nothing",
			body: "<!-- legion: {\"session\":\"ses-reviewer\",\"head\":\"" + head[:39] + "\"} -->",
			want: "",
		},
		{
			name: "an upper-case head names nothing: GitHub spells every head it reports in lower case",
			body: "<!-- legion: {\"session\":\"ses-reviewer\",\"head\":\"8848896DA05779C592221B1199EE55DDB11CC3F3\"} -->",
			want: "",
		},
		{name: "a footer with no head", body: "<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\"} -->", want: ""},
		{name: "no footer", body: "Ship it.", want: ""},
		{
			name: "two footers; the last one wins",
			body: "<!-- legion: {\"session\":\"ses-reviewer\",\"head\":\"1111111111111111111111111111111111111111\"} -->\n\nedit: re-read at the new head.\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"head\":\"" + head + "\"} -->",
			want: head,
		},
		{name: "broken JSON", body: "<!-- legion: {\"session\":\"ses-reviewer\",\"head\":\"" + head + "\" -->", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := PullRequestReview{Body: tc.body}
			if got := review.LegionHead(); got != tc.want {
				t.Fatalf("LegionHead() of %q = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// The reviewer's footer names the entry of the round its review is of, the number its task names
// (reviewround.EntryLabel), and only a review that names the entry that stands moves the
// commit the reviewer read (workflow's review). LegionEntry reads it as a positive whole number
// written as a JSON integer or as a JSON string of digits, which is the form the reviewer's texts
// give (`"entry":"<entry>"`), so a placeholder left as it is keeps the footer JSON. It answers 0
// for anything else. An entry it cannot read costs the entry alone: the session and the head of
// the same footer still read, since they decide the round. A footer that is not JSON at all, an
// unquoted placeholder `"entry":<entry>` among them, still reads as no footer: its session and its
// head read empty too.
func TestLegionEntryReadsTheEntryTheReviewSaysItIsOf(t *testing.T) {
	const head = "8848896da05779c592221b1199ee55ddb11cc3f3"
	footer := func(entry string) string {
		return "Round 2: APPROVE.\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\",\"head\":\"" + head + "\",\"entry\":" + entry + "} -->"
	}
	for _, tc := range []struct {
		name string
		body string
		want int64
		// notJSON is a body whose last footer is no JSON object, or that carries no footer: it
		// names no session and no head either.
		notJSON bool
	}{
		{name: "the entry the review is of", body: footer("7"), want: 7},
		{name: "the entry as a string of digits", body: footer("\"7\""), want: 7},
		{name: "the entry with space around it", body: "<!-- legion: {\"session\":\"ses-reviewer\",\"head\":\"" + head + "\", \"entry\" :  12 } -->", want: 12},
		{name: "the largest entry int64 holds", body: footer("9223372036854775807"), want: 9223372036854775807},
		{name: "the largest entry int64 holds as a string", body: footer("\"9223372036854775807\""), want: 9223372036854775807},
		{name: "a footer with no entry", body: "<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\",\"head\":\"" + head + "\"} -->", want: 0},
		{name: "no footer", body: "Ship it.", want: 0, notJSON: true},
		{name: "zero", body: footer("0"), want: 0},
		{name: "a negative entry", body: footer("-3"), want: 0},
		{name: "a fraction", body: footer("3.5"), want: 0},
		{name: "a whole number written as a fraction", body: footer("3.0"), want: 0},
		{name: "an exponent", body: footer("3e0"), want: 0},
		{name: "one past int64", body: footer("9223372036854775808"), want: 0},
		{name: "one past int64 as a string", body: footer("\"9223372036854775808\""), want: 0},
		{name: "the placeholder left unfilled", body: footer("\"<entry>\""), want: 0},
		{name: "an empty string", body: footer("\"\""), want: 0},
		{name: "digits and a word", body: footer("\"07x\""), want: 0},
		{name: "zero as a string", body: footer("\"0\""), want: 0},
		{name: "a negative entry as a string", body: footer("\"-1\""), want: 0},
		{name: "a signed entry as a string", body: footer("\"+7\""), want: 0},
		{name: "a fraction as a string", body: footer("\"1.5\""), want: 0},
		{name: "null", body: footer("null"), want: 0},
		{name: "true", body: footer("true"), want: 0},
		{name: "an array", body: footer("[]"), want: 0},
		{name: "an object", body: footer("{\"n\":3}"), want: 0},
		{
			name: "two footers; the last one wins",
			body: footer("2") + "\n\nedit: re-read in the next round.\n\n" + footer("3"),
			want: 3,
		},
		{name: "broken JSON", body: "<!-- legion: {\"session\":\"ses-reviewer\",\"entry\":3 -->", want: 0, notJSON: true},
		{name: "an unquoted placeholder", body: footer("<entry>"), want: 0, notJSON: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := PullRequestReview{Body: tc.body}
			if got := review.LegionEntry(); got != tc.want {
				t.Fatalf("LegionEntry() of %q = %d, want %d", tc.body, got, tc.want)
			}
			if tc.notJSON {
				if review.LegionSession() != "" || review.LegionHead() != "" {
					t.Fatalf("the body %q reads the session %q and the head %q, want neither: it carries no footer that is JSON",
						tc.body, review.LegionSession(), review.LegionHead())
				}
				return
			}
			if review.LegionSession() != "ses-reviewer" || review.LegionHead() != head {
				t.Fatalf("the footer %q reads the session %q and the head %q, want ses-reviewer and %s: an entry costs no more than itself",
					tc.body, review.LegionSession(), review.LegionHead(), head)
			}
		})
	}
}

// A carried approval is the review Dispatch writes in a person's name when their answer to a
// decision block was the whole change since the version they approved; its reason opens with
// CarriedApprovalReasonPrefix, mirrored from Dispatch's carryReason
// (packages/envoy/internal/dispatch/docs/approval_carry.go). It is not a person's new act, so the
// workflow queues no parked root for it. An approval given with no reason, and a review with any
// other reason, are not carries.
func TestCarriedApprovalReadsTheReasonsPrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		want   bool
	}{
		{"Dispatch's carried reason", "Carried forward from the approval of version 3: the answer to a decision block was the whole change since it.", true},
		{"the prefix alone", CarriedApprovalReasonPrefix, true},
		{"no reason", "", false},
		{"a reviewer's own reason", "Looks right; the rollback path is named.", false},
		{"the prefix later in the reason", "Not Carried forward from the approval of version 3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := DispatchArtifact{Kind: DispatchArtifactApproved, Reason: tc.reason}
			if got := review.CarriedApproval(); got != tc.want {
				t.Fatalf("CarriedApproval() of %q = %t, want %t", tc.reason, got, tc.want)
			}
		})
	}
}
