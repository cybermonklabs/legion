package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testdata/dispatch/issue-updated-link-own-repository.json and
// testdata/dispatch/issue-updated-link-other-repository.json are real Dispatch events: an issue's
// events of seq 3 (id 1154219, which links a pull request in the issue's own repository, #5481)
// and seq 4 (id 1154334, which links that pull request and #6990, in another repository), each
// read on 2026-11-27 from the issue's event log with the deployment's Dispatch token, <KEY> being
// the issue's key in that deployment and N being 3 and then 4:
//
//	curl -fsS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<KEY>/events" |
//	  jq -c '.[] | select(.seq == N) | del(.actor.origin, .actor.owner, .payload.assignee)'
//
// Each event became the string payload of the Envoy envelope testdata/dispatch/issue-updated.json
// shows: the topic notifications.dispatch.issue.<KEY>.issue.updated, the payload summary
// `<KEY> issue updated`, and event_id and dedupe_key dispatch-<id>. This repository is public, so
// the filter drops the event actor's origin and owner and the issue's assignee, which held an email
// address, a machine name, a home path and text that names a person, and then each envelope went
// through this rewrite, the event inside it as JSON and the envelope around it:
//
//	def neutral: walk(if type == "string" then
//	    gsub("<KEY>"; "ACME-5768") | gsub("<REPO>"; "acme/widgets") | gsub("<OTHER>"; "acme/deploy")
//	    | gsub("<ACTOR>"; "01900000-0000-7000-8000-000000000001")
//	    | gsub("<CREATOR>"; "01900000-0000-7000-8000-000000000002")
//	    | if . == "<P>" then "ACME" else . end
//	  else . end);
//	.payload |= (fromjson | walk(if type == "object" then del(.origin) else . end) | neutral | tojson) | neutral
//
// <P> is the project's key, <REPO> the issue's repository, <OTHER> the repository of its second
// link, and <ACTOR> and <CREATOR> the ids of the sessions that wrote the events and created the
// issue. So the rewrite drops every origin object left (the creator's, which named its session's
// machine, its working directory and its title); in every string the project's key became the
// neutral ACME, as AGENTS.md asks, so the issue keeps its number, and the two repositories became
// acme/widgets and acme/deploy; and the two session ids became neutral ids, since each named a
// session of the deployment that may still be live. Every other byte of the event is as the filter
// left it. The decoder reads none of what the filter and the rewrite dropped; it reads the actor's
// id only as a session that is not the daemon's, which the neutral id still is. internal/dispatch's
// resolved_test.go treats its captures the same way. Every other row below is built from these
// two and from the captured envelopes of testdata/github, and says how.

const (
	linkedIssue        = "ACME-5768"
	linkedIssueSubject = "notifications.dispatch.issue.ACME-5768.issue.updated"
	// linkedHead is the head branch of the pull request ACME-5768 links in another repository,
	// a branch no issue's name gives (claim.BranchIssue).
	linkedHead = "fix/several-pull-requests"
)

var (
	ownRepository   = ghrepo.MustParse("acme/widgets")
	otherRepository = ghrepo.MustParse("acme/deploy")
	// linkedRepositories is what GitHub intake hears for ACME-5768: its own repository and the
	// one its link names.
	linkedRepositories = []ghrepo.Repository{ownRepository, otherRepository}
)

// The pull requests of an issue are the pull requests of its plan tasks, on its branches, and the
// pull requests it links by their URL, in any repository; no other (D2 of dispatch://INFRA-5910).
// The intake's half of that rule is what it decodes. An issue event names the issue's links
// (external_links): each URL of the form https://github.com/<owner>/<name>/pull/<number>, the rule
// of Dispatch's linkedPullRequestTerms with the repository lowercased, counts, the issue's own
// repository too, and any other URL is no pull request. An event that carries no external_links
// says nothing of the links, which is never read as no links. The pull request events and the
// pushes of a branch that no issue's name gives decode to facts only for a pull request an issue
// links, by the decoder's index of the links (linkIndex): such a pull request's events are its
// facts off legion/<KEY>, a push of its head branch is a Push, and a fork's pull request is no
// fact, linked or not. A pull request that only names the issue, in its body, its title, its
// branch or a commit message, is in no set and decodes to no fact of it, and its event costs the
// consumer no transaction. The index is loaded whole, one issue's links replaced and a link's
// head branch set, and it is read under both names of a configured rename. A read of a linked
// pull request is the fact of what GitHub answered for it.
//
// The faults: the decode reads no external_links, or reads an absent field as no links; it takes
// a URL of any shape, or one into the issue's own repository not at all; it decodes every pull
// request of a heard repository, or none off legion/<KEY>; a push of a linked head is a base push;
// the fork check is skipped for a linked pull request; an unlinked pull request's event opens a
// transaction. The controls: the linked pull request's own events, which decode and are applied.
func TestThePullRequestsOfAnIssueAreItsTasksAndItsLinks(t *testing.T) {
	t.Parallel()
	ownLink := PullRequestLink{Repo: "acme/widgets", Number: 5481}
	otherLink := PullRequestLink{Repo: "acme/deploy", Number: 6990}

	t.Run("the links an issue event names", func(t *testing.T) {
		seq3 := readTestdata(t, "testdata/dispatch/issue-updated-link-own-repository.json")
		seq4 := readTestdata(t, "testdata/dispatch/issue-updated-link-other-repository.json")
		closed := readTestdata(t, "testdata/dispatch/issue-closed.json")
		created := readTestdata(t, "testdata/dispatch/issue-created.json")
		bothLinks := `[{"url":"https://github.com/acme/widgets/pull/5481","kind":"url"},{"url":"https://github.com/acme/deploy/pull/6990","kind":"url"}]`
		for _, tc := range []struct {
			name, subject, project string
			data                   []byte
			links                  []PullRequestLink
			known, unread          bool
		}{
			{name: "a pull request of the issue's own repository (the capture of seq 3)", data: seq3, links: []PullRequestLink{ownLink}, known: true},
			{name: "pull requests of two repositories (the capture of seq 4)", data: seq4, links: []PullRequestLink{ownLink, otherLink}, known: true},
			// The rows below are the capture of seq 4 with its external_links alone edited.
			{name: "the link of the other repository removed", data: withExternalLinks(t, seq4, `[{"url":"https://github.com/acme/widgets/pull/5481","kind":"url"}]`),
				links: []PullRequestLink{ownLink}, known: true},
			{name: "every link removed", data: withExternalLinks(t, seq4, `[]`), known: true},
			{name: "links that are no pull request", data: withExternalLinks(t, seq4, `[`+
				`{"url":"https://github.com/acme/deploy/issues/6990","kind":"url"},`+
				`{"url":"https://github.com/acme/deploy/pull/6990/files","kind":"url"},`+
				`{"url":"http://github.com/acme/deploy/pull/6990","kind":"url"},`+
				`{"url":"https://example.com/acme/deploy/pull/6990","kind":"url"},`+
				`{"url":"https://github.com/acme/deploy/pull/0","kind":"url"},`+
				`{"url":"https://github.com/acme/deploy/pull/1234567890","kind":"url"},`+
				`{"url":"https://github.com/acme/./pull/7","kind":"url"},`+
				`{"url":"https://github.com/acme/../pull/7","kind":"url"},`+
				`{"url":"https://github.com/-acme/widgets/pull/7","kind":"url"},`+
				`{"url":"https://github.com/acme/widgets/pull/5481","kind":"url"}]`),
				links: []PullRequestLink{ownLink}, known: true},
			{name: "one pull request linked in two spellings", data: withExternalLinks(t, seq4, `[`+
				`{"url":"https://github.com/Acme/Deploy/pull/6990","kind":"url"},`+
				`{"url":"https://github.com/acme/deploy/pull/6990","kind":"url"}]`),
				links: []PullRequestLink{otherLink}, known: true},
			{name: "no external_links field", data: withExternalLinks(t, seq4, "")},
			{name: "external_links null, as Dispatch writes an issue whose links it did not read", data: withExternalLinks(t, seq4, `null`)},
			{name: "external_links that are no list of links", data: withExternalLinks(t, seq4, `"https://github.com/acme/deploy/pull/6990"`), unread: true},
			// The captured issue.created and issue.closed of CAPTURE-4 with their external_links alone
			// edited: every issue event names the links.
			{name: "an issue.created", subject: "notifications.dispatch.issue.CAPTURE-4.issue.created", project: "CAPTURE",
				data: withExternalLinks(t, created, bothLinks), links: []PullRequestLink{ownLink, otherLink}, known: true},
			{name: "an issue.closed", subject: "notifications.dispatch.issue.CAPTURE-4.issue.closed", project: "CAPTURE",
				data: withExternalLinks(t, closed, bothLinks), links: []PullRequestLink{ownLink, otherLink}, known: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				subject, project := linkedIssueSubject, "ACME"
				if tc.subject != "" {
					subject, project = tc.subject, tc.project
				}
				decoded, err := decodeMessage(subject, project, nil, nil, tc.data)
				if err != nil {
					t.Fatalf("decode: %v, want the issue's fact", err)
				}
				issue, ok := decoded.Fact.(DispatchIssue)
				if !ok {
					t.Fatalf("fact = %#v, want a DispatchIssue", decoded.Fact)
				}
				if !slices.Equal(issue.Links, tc.links) || issue.LinksKnown != tc.known {
					t.Fatalf("the event names links %v, known %t; want %v, known %t", issue.Links, issue.LinksKnown, tc.links, tc.known)
				}
				if unread := len(decoded.Unread) != 0; unread != tc.unread {
					t.Fatalf("unread = %q, want unread %t", decoded.Unread, tc.unread)
				}
			})
		}
	})

	t.Run("the events a link decodes", func(t *testing.T) {
		links := newLinkIndex(ghrepo.Renames{})
		links.load([]LinkedPullRequest{
			{Issue: linkedIssue, Repo: "acme/widgets", Number: 5481, Branch: "legion/" + linkedIssue},
			{Issue: linkedIssue, Repo: "acme/deploy", Number: 6990, Branch: linkedHead},
		})
		updatedAt := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
		linked := map[string]string{"repo": "acme/deploy", "head_repo": "acme/deploy", "head_ref": linkedHead, "number": "6990",
			"url": "https://github.com/acme/deploy/pull/6990", "body": "The second half of the change."}
		// unlinked is a pull request of the other repository that ACME-5768 does not link.
		unlinked := map[string]string{"repo": "acme/deploy", "head_repo": "acme/deploy", "head_ref": "fix/other", "number": "7001",
			"url": "https://github.com/acme/deploy/pull/7001", "body": "An unrelated change.", "title": "An unrelated change"}
		onBranch := func(ref string, fields map[string]string) map[string]string {
			return with(fields, map[string]string{"repo": "acme/deploy", "ref": "refs/heads/" + ref})
		}
		for _, tc := range []struct {
			name, file string
			fields     map[string]string
			want       Fact
		}{
			// The pull request of the issue's task, on its branch in its own repository, decodes as
			// it does today.
			{name: "the task's pull request on legion/ACME-5768", file: "pr-opened.json", fields: map[string]string{"repo": "acme/widgets",
				"head_repo": "acme/widgets", "head_ref": "legion/" + linkedIssue, "number": "5481", "url": "https://github.com/acme/widgets/pull/5481"},
				want: PullRequestOpened{Repo: "acme/widgets", Number: 5481, Branch: "legion/" + linkedIssue, Base: "main", HeadSHA: "head-captured",
					HeadRepo: "acme/widgets", Body: "Dispatch: LEGION-208", URL: "https://github.com/acme/widgets/pull/5481", UpdatedAt: updatedAt}},
			{name: "the linked pull request opened off legion/ACME-5768", file: "pr-opened.json", fields: linked,
				want: PullRequestOpened{Repo: "acme/deploy", Number: 6990, Branch: linkedHead, Base: "main", HeadSHA: "head-captured",
					HeadRepo: "acme/deploy", Body: "The second half of the change.", URL: "https://github.com/acme/deploy/pull/6990", UpdatedAt: updatedAt}},
			{name: "the linked pull request synchronized", file: "pr-synchronized.json", fields: linked,
				want: PullRequestSynchronized{Repo: "acme/deploy", Number: 6990, Branch: linkedHead, Before: "head-before", Sender: "legion-implementer[bot]",
					HeadSHA: "head-captured", Base: "main", Body: "The second half of the change.", UpdatedAt: updatedAt}},
			{name: "the linked pull request closed", file: "pr-closed.json", fields: linked,
				want: PullRequestClosed{Repo: "acme/deploy", Number: 6990, HeadSHA: "head-captured", UpdatedAt: updatedAt}},
			{name: "the linked pull request merged", file: "pr-merged.json", fields: linked,
				want: PullRequestMerged{Repo: "acme/deploy", Number: 6990, MergeSHA: "merge-captured", HeadSHA: "head-captured"}},
			// GitHub compares names without case, and the link is held lowercased; the fact names
			// the repository as GitHub intake hears it.
			{name: "the linked pull request in another spelling of its repository", file: "pr-closed.json",
				fields: with(linked, map[string]string{"repo": "Acme/Deploy", "head_repo": "Acme/Deploy"}),
				want:   PullRequestClosed{Repo: "acme/deploy", Number: 6990, HeadSHA: "head-captured", UpdatedAt: updatedAt}},
			{name: "a push of the linked pull request's head branch", file: "push.json", fields: onBranch(linkedHead, nil),
				want: Push{Repo: "acme/deploy", Branch: linkedHead, Before: "before-captured", After: "head-captured", Pusher: "author", Sender: "author"}},
			// A deletion names no head, and the branch may be the base of another pull request, so
			// it is the base push every other branch's deletion is.
			{name: "the deletion of the linked pull request's head branch", file: "push.json", fields: onBranch(linkedHead, map[string]string{"after": record.NoCommit}),
				want: BasePush{Repo: "acme/deploy", Branch: linkedHead, After: record.NoCommit}},
			{name: "a push of a branch no link heads", file: "push.json", fields: onBranch("main", nil),
				want: BasePush{Repo: "acme/deploy", Branch: "main", After: "head-captured"}},
			{name: "a fork's pull request on legion/ACME-5768", file: "pr-opened.json", fields: map[string]string{"repo": "acme/widgets",
				"head_repo": "stranger/legion", "head_ref": "legion/" + linkedIssue, "number": "5999"}},
			{name: "a fork's pull request the issue links", file: "pr-opened.json", fields: map[string]string{"repo": "acme/widgets",
				"head_repo": "stranger/legion", "head_ref": "patch-1", "number": "5481"}},
			{name: "an unlinked pull request of the other repository that names the issue in its body", file: "pr-opened.json",
				fields: with(unlinked, map[string]string{"body": "Dispatch: " + linkedIssue})},
			{name: "one that names the issue in its title", file: "pr-opened.json",
				fields: with(unlinked, map[string]string{"title": linkedIssue + ": the second half of the change"})},
			{name: "one that names the issue in its branch", file: "pr-opened.json",
				fields: with(unlinked, map[string]string{"head_ref": "fix/" + linkedIssue})},
			// The commit's message names the issue: the push of its branch is the base push of a
			// branch no issue's name gives and no link heads, and the synchronize it makes is no
			// fact.
			{name: "one whose commit message names the issue: its push", file: "push.json",
				fields: onBranch("fix/other", map[string]string{"head_subject": linkedIssue + ": the second half of the change"}),
				want:   BasePush{Repo: "acme/deploy", Branch: "fix/other", After: "head-captured"}},
			{name: "one whose commit message names the issue: its synchronize", file: "pr-synchronized.json", fields: unlinked},
		} {
			t.Run(tc.name, func(t *testing.T) {
				decoded, err := decodeMessage("notifications.github.acme.deploy.pr.6990", "ACME", linkedRepositories, links,
					githubEvent(t, tc.file, "linked-event", tc.fields))
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !reflect.DeepEqual(decoded.Fact, tc.want) {
					t.Fatalf("fact = %#v, want %#v", decoded.Fact, tc.want)
				}
				if decoded.NoHeadRepo {
					t.Fatal("the event is told as one that names no head repository; it names one")
				}
			})
		}
	})

	t.Run("the index loaded, replaced, and told a head", func(t *testing.T) {
		links := newLinkIndex(ghrepo.Renames{})
		links.load([]LinkedPullRequest{
			{Issue: linkedIssue, Repo: "acme/deploy", Number: 6990, Branch: linkedHead},
			{Issue: linkedIssue, Repo: "acme/deploy", Number: 7100, Branch: "fix/kept"},
			{Issue: linkedIssue, Repo: "acme/deploy", Number: 7101, Branch: "fix/kept"},
			{Issue: "ACME-5769", Repo: "acme/deploy", Number: 7500, Branch: "fix/neighbour"},
		})
		decodes := func(t *testing.T, number int, head string, wantPullRequest, wantPush bool) {
			t.Helper()
			fields := map[string]string{"repo": "acme/deploy", "head_repo": "acme/deploy", "head_ref": head,
				"number": fmt.Sprint(number), "ref": "refs/heads/" + head}
			opened, err := decodeMessage("notifications.github.acme.deploy.pr.x", "ACME", linkedRepositories, links, githubEvent(t, "pr-opened.json", "opened", fields))
			if err != nil {
				t.Fatalf("decode the opened of #%d: %v", number, err)
			}
			if _, got := opened.Fact.(PullRequestOpened); got != wantPullRequest {
				t.Errorf("the opened of #%d on %s decodes to %#v; want a fact %t", number, head, opened.Fact, wantPullRequest)
			}
			pushed, err := decodeMessage("notifications.github.acme.deploy.push.x", "ACME", linkedRepositories, links, githubEvent(t, "push.json", "pushed", fields))
			if err != nil {
				t.Fatalf("decode the push of %s: %v", head, err)
			}
			if _, got := pushed.Fact.(Push); got != wantPush {
				t.Errorf("the push of %s decodes to %#v; want a Push %t, else a BasePush", head, pushed.Fact, wantPush)
			}
		}
		decodes(t, 6990, linkedHead, true, true)
		decodes(t, 7100, "fix/kept", true, true)

		// ACME-5768's links replaced: #6990 removed, #7101 removed beside #7100 on one head, and
		// #7200 added, whose head no read has told yet. Another issue's link stays.
		links.replace(linkedIssue, []PullRequestLink{{Repo: "Acme/Deploy", Number: 7100}, {Repo: "acme/deploy", Number: 7200}})
		decodes(t, 6990, linkedHead, false, false)
		decodes(t, 7101, "fix/kept", false, true)
		decodes(t, 7100, "fix/kept", true, true)
		decodes(t, 7200, "fix/new", true, false)
		decodes(t, 7500, "fix/neighbour", true, true)

		// A link read tells #7200's head, and a head told of a pull request no issue links adds
		// nothing.
		links.setHead("acme/deploy", 7200, "fix/new")
		links.setHead("acme/deploy", 7300, "fix/stranger")
		decodes(t, 7200, "fix/new", true, true)
		decodes(t, 7300, "fix/stranger", false, false)

		// A boot loads the whole set again.
		links.load(nil)
		decodes(t, 7100, "fix/kept", false, false)
		decodes(t, 7500, "fix/neighbour", false, false)
	})

	t.Run("a link under either name of a configured rename", func(t *testing.T) {
		renames := ghrepo.MustRenames(ghrepo.MustRename("acme/gadgets", "acme/widgets"))
		heard := []ghrepo.Repository{ghrepo.MustParse("acme/gadgets"), ownRepository}
		links := newLinkIndex(renames)
		links.load([]LinkedPullRequest{
			{Issue: linkedIssue, Repo: "acme/gadgets", Number: 77, Branch: "fix/recorded-before"},
			{Issue: linkedIssue, Repo: "acme/widgets", Number: 78, Branch: "fix/recorded-after"},
		})
		for _, tc := range []struct {
			repo, head string
			number     int
		}{{"acme/widgets", "fix/recorded-before", 77}, {"acme/gadgets", "fix/recorded-after", 78}} {
			fields := map[string]string{"repo": tc.repo, "head_repo": tc.repo, "head_ref": tc.head, "number": fmt.Sprint(tc.number), "ref": "refs/heads/" + tc.head}
			opened, err := decodeMessage("notifications.github.x.pr", "ACME", heard, links, githubEvent(t, "pr-opened.json", "opened", fields))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got, ok := opened.Fact.(PullRequestOpened); !ok || got.Repo != tc.repo || got.Number != tc.number {
				t.Errorf("the opened of %s#%d decodes to %#v; want the linked pull request's opened", tc.repo, tc.number, opened.Fact)
			}
			pushed, err := decodeMessage("notifications.github.x.push", "ACME", heard, links, githubEvent(t, "push.json", "pushed", fields))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got, ok := pushed.Fact.(Push); !ok || got.Branch != tc.head {
				t.Errorf("the push of %s in %s decodes to %#v; want a Push of the linked head", tc.head, tc.repo, pushed.Fact)
			}
		}
	})

	t.Run("the fact of a link read", func(t *testing.T) {
		read := func(t *testing.T, answer string) githubrest.PullRequest {
			t.Helper()
			var pull githubrest.PullRequest
			if err := json.Unmarshal([]byte(answer), &pull); err != nil {
				t.Fatalf("decode GitHub's answer: %v", err)
			}
			return pull
		}
		const answer = `{"number":6990,"state":%q,"merged":%t,"merge_commit_sha":"merge-6990","html_url":"https://github.com/acme/deploy/pull/6990",` +
			`"base":{"ref":"main"},"head":{"ref":"fix/several-pull-requests","sha":"head-6990","repo":{"full_name":%q}},"updated_at":"2026-11-25T03:50:00Z"}`
		updatedAt := time.Date(2026, 11, 25, 3, 50, 0, 0, time.UTC)
		for _, tc := range []struct {
			name, state, headRepo string
			merged                bool
			want                  PullRequestLinkReadDone
		}{
			// GitHub's merge_commit_sha of an open pull request is its test merge, no merge.
			{name: "open", state: "open", headRepo: "acme/deploy", want: PullRequestLinkReadDone{Issue: linkedIssue, Repo: "acme/deploy", Number: 6990,
				State: record.PullRequestOpen, HeadSHA: "head-6990", HeadRepo: "acme/deploy", Branch: linkedHead, Base: "main",
				URL: "https://github.com/acme/deploy/pull/6990", UpdatedAt: updatedAt}},
			{name: "merged", state: "closed", merged: true, headRepo: "acme/deploy", want: PullRequestLinkReadDone{Issue: linkedIssue, Repo: "acme/deploy",
				Number: 6990, State: record.PullRequestMerged, HeadSHA: "head-6990", HeadRepo: "acme/deploy", Branch: linkedHead, Base: "main",
				MergeSHA: "merge-6990", URL: "https://github.com/acme/deploy/pull/6990", UpdatedAt: updatedAt}},
			{name: "closed unmerged", state: "closed", headRepo: "acme/deploy", want: PullRequestLinkReadDone{Issue: linkedIssue, Repo: "acme/deploy",
				Number: 6990, State: record.PullRequestClosed, HeadSHA: "head-6990", HeadRepo: "acme/deploy", Branch: linkedHead, Base: "main",
				URL: "https://github.com/acme/deploy/pull/6990", UpdatedAt: updatedAt}},
			// The head of a fork, which the workflow refuses the link for (record.LinkRefusedFork).
			{name: "a fork's", state: "open", headRepo: "stranger/deploy", want: PullRequestLinkReadDone{Issue: linkedIssue, Repo: "acme/deploy", Number: 6990,
				State: record.PullRequestOpen, HeadSHA: "head-6990", HeadRepo: "stranger/deploy", Branch: linkedHead, Base: "main",
				URL: "https://github.com/acme/deploy/pull/6990", UpdatedAt: updatedAt}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pull := read(t, fmt.Sprintf(answer, tc.state, tc.merged, tc.headRepo))
				fact, id := LinkRead("ACME", linkedIssue, "acme/deploy", pull)
				if !reflect.DeepEqual(fact, tc.want) {
					t.Fatalf("the read's fact = %#v, want %#v", fact, tc.want)
				}
				if again, sameID := LinkRead("ACME", linkedIssue, "acme/deploy", pull); !reflect.DeepEqual(again, fact) || sameID != id {
					t.Fatalf("the same answer read again is %#v under %q, want the same fact under %q", again, sameID, id)
				}
				moved := pull
				moved.Head.SHA, moved.UpdatedAt = "head-6990-moved", pull.UpdatedAt.Add(time.Minute)
				if _, movedID := LinkRead("ACME", linkedIssue, "acme/deploy", moved); movedID == id {
					t.Fatalf("a read of a moved head is applied under %q, the id of the earlier read; want a new fact", movedID)
				}
			})
		}
		if got, ok := IssueOf(PullRequestLinkReadDone{Issue: linkedIssue}); !ok || got != linkedIssue {
			t.Fatalf("IssueOf(PullRequestLinkReadDone) = %q, %t; want %s: the read is of the issue's link", got, ok, linkedIssue)
		}
	})

	t.Run("an unlinked pull request's event off legion/KEY costs no transaction", func(t *testing.T) {
		pool := migratedPool(t)
		createWrites(t, pool)
		locks := &factLocks{}
		traced := pool.Config().Copy()
		traced.ConnConfig.Tracer = locks
		counted, err := pgxpool.NewWithConfig(context.Background(), traced)
		if err != nil {
			t.Fatalf("open the counted pool: %v", err)
		}
		t.Cleanup(counted.Close)
		js, stream := testJetStream(t)
		spec := consumerSpec(&lockedBuffer{})
		spec.Repositories = linkedRepositories
		// A hang guard, not the subject, as in TestAnEventTheDaemonCannotActOnIsAcknowledgedWithNoTransaction:
		// the row counts the fact lock's statements exactly, so no delivery may fall due again.
		spec.AckWait = testwait.Remaining(t)
		consumers, err := OpenConsumers(context.Background(), js, counted, spec)
		if err != nil {
			t.Fatalf("OpenConsumers: %v", err)
		}
		consumers.LoadLinks([]LinkedPullRequest{{Issue: linkedIssue, Repo: "acme/deploy", Number: 6990, Branch: linkedHead}})
		stop := runConsumers(t, consumers, handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
			_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, reflect.TypeOf(fact).Name())
			return Result{}, err
		}))
		defer stop()

		const prSubject, pushSubject = "notifications.github.acme.deploy.pr.6990", "notifications.github.acme.deploy.push.branch.fix"
		linked := map[string]string{"repo": "acme/deploy", "head_repo": "acme/deploy", "head_ref": linkedHead, "number": "6990"}
		for i, event := range []struct {
			name, subject string
			body          []byte
			// then is the change of the links that follows the event, nil for none.
			then func()
			// applied is the kind of each fact the consumer applied after the event.
			applied []string
		}{
			{name: "an unlinked pull request opened off legion/ACME-5768, naming the issue in its body", subject: prSubject,
				body: githubEvent(t, "pr-opened.json", "unlinked-opened", map[string]string{"repo": "acme/deploy", "head_repo": "acme/deploy",
					"head_ref": "fix/other", "number": "7001", "body": "Dispatch: " + linkedIssue})},
			{name: "the linked pull request opened", subject: prSubject, body: githubEvent(t, "pr-opened.json", "linked-opened", linked),
				then: func() { consumers.ReplaceLinks(linkedIssue, nil) }, applied: []string{"PullRequestOpened"}},
			{name: "its synchronize after its link was removed", subject: prSubject, body: githubEvent(t, "pr-synchronized.json", "unlinked-synchronized", linked),
				then: func() {
					consumers.ReplaceLinks(linkedIssue, []PullRequestLink{otherLink})
					consumers.SetLinkHead("acme/deploy", 6990, linkedHead)
				}, applied: []string{"PullRequestOpened"}},
			{name: "a push of its head branch once it is linked again and read", subject: pushSubject,
				body:    githubEvent(t, "push.json", "linked-push", map[string]string{"repo": "acme/deploy", "ref": "refs/heads/" + linkedHead}),
				applied: []string{"PullRequestOpened", "Push"}},
		} {
			waitAcknowledged(t, stream, githubConsumerName(spec.Project), publishAt(t, js, event.subject, event.body))
			if got := writeHandlers(t, pool); !slices.Equal(got, event.applied) {
				t.Fatalf("after %s (event %d) the consumer applied %v, want %v", event.name, i+1, got, event.applied)
			}
			if taken, processed := locks.taken.Load(), processedEvents(t, pool); taken != int64(len(event.applied)) || processed != len(event.applied) {
				t.Fatalf("after %s the fact lock was taken %d times and %d events were processed, want %d of each: one transaction for each fact applied, none for an unlinked pull request's event",
					event.name, taken, processed, len(event.applied))
			}
			if event.then != nil {
				event.then()
			}
		}
	})
}

// readTestdata is the file at path, under the package's directory.
func readTestdata(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// withExternalLinks is the Dispatch envelope with the issue event's external_links, and nothing
// else, replaced by links, a JSON value, or removed where links is "". Every other field of the
// envelope, the event and the issue keeps its bytes.
func withExternalLinks(t *testing.T, envelope []byte, links string) []byte {
	t.Helper()
	return editPayload(t, envelope, func(event map[string]json.RawMessage) {
		var issue map[string]json.RawMessage
		if err := json.Unmarshal(event["payload"], &issue); err != nil {
			t.Fatalf("decode the issue of the event: %v", err)
		}
		if _, carried := issue["external_links"]; !carried {
			t.Fatal("the event carries no external_links to replace")
		}
		if links == "" {
			delete(issue, "external_links")
		} else {
			issue["external_links"] = json.RawMessage(links)
		}
		event["payload"] = marshalled(t, issue)
	})
}

// githubEvent is the captured GitHub envelope testdata/github/name with each payload field of
// fields set, under its own delivery id, id, which the envelope's event id and dedupe key name.
func githubEvent(t *testing.T, name, id string, fields map[string]string) []byte {
	t.Helper()
	data := editPayload(t, capturedGitHubEnvelope(t, name), func(payload map[string]json.RawMessage) {
		for key, value := range fields {
			payload[key] = marshalled(t, value)
		}
	})
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	envelope["event_id"], envelope["source_event_id"], envelope["dedupe_key"] = marshalled(t, id), marshalled(t, id), marshalled(t, "github."+id)
	return marshalled(t, envelope)
}

// editPayload is the Envoy envelope with its string payload, a JSON object, decoded, edited by
// edit, and encoded again.
func editPayload(t *testing.T, envelope []byte, edit func(map[string]json.RawMessage)) []byte {
	t.Helper()
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(envelope, &outer); err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	var text string
	if err := json.Unmarshal(outer["payload"], &text); err != nil {
		t.Fatalf("decode the envelope's payload: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode the payload: %v", err)
	}
	edit(payload)
	outer["payload"] = marshalled(t, string(marshalled(t, payload)))
	return marshalled(t, outer)
}

func marshalled(t *testing.T, value any) []byte {
	t.Helper()
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatalf("encode %#v: %v", value, err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// with is fields with each of changes set over it, a new map.
func with(fields, changes map[string]string) map[string]string {
	out := make(map[string]string, len(fields)+len(changes))
	for key, value := range fields {
		out[key] = value
	}
	for key, value := range changes {
		out[key] = value
	}
	return out
}
