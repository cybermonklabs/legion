package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// notReadChecks is a Verification document with two `before_merge` checks. Both checks compare
// with the heads of the pull requests of the issue, so each check can name a head that Dispatch
// has not read.
const notReadChecks = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\n" +
	"The server records a result against a check.\n" +
	":::\n\n" +
	":::check{#k2 surface=\"cli\" run=\"go test ./...\"}\n" +
	"A second check compares with the same heads.\n" +
	":::\n"

const (
	// notReadOwnHead is the head of #80, a pull request whose head Dispatch has read.
	notReadOwnHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// notReadOtherCommit is a commit that no pull request of these tests has as its head.
	notReadOtherCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// notReadKnownHead is the head of #84, a merged pull request whose issue is known.
	notReadKnownHead = "8484848484848484848484848484848484848484"
)

// notReadUpdatedAt is the GitHub update time of each pull request that these tests store. The
// results row and the texts row both have it, so the texts are current.
var notReadUpdatedAt = time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)

// notReadPull is one pull request of acme/widgets as these tests store it. It has a results row
// that a read of GitHub wrote. Unless noTexts is set, it also has a texts row with the same
// head, branch, base, title and update time. The texts name no key unless the branch names one.
// A nil messages means that Dispatch has not read the commit messages. An empty stored leaves
// the issue of the results row null.
type notReadPull struct {
	number   int
	head     string
	branch   string
	base     string
	state    string
	stored   string
	messages []string
	noTexts  bool
}

// storeNotReadPull writes the rows of pull, as the writer of facts and a read of GitHub leave
// them.
func storeNotReadPull(t *testing.T, database *store.Store, pull notReadPull) {
	t.Helper()
	ctx := context.Background()
	var stored *string
	source := "none"
	if pull.stored != "" {
		stored, source = &pull.stored, "branch"
	}
	url := fmt.Sprintf("https://github.com/acme/widgets/pull/%d", pull.number)
	if _, err := database.Pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, url, title,
			event_at, checked_at, github_read_at, issue_key, attribution_source)
		values ('acme/widgets', $1, $2, $3, $4, $5, $6, 'A change', $7, now(), now(), $8, $9)`,
		pull.number, pull.head, pull.branch, pull.base, pull.state, url, notReadUpdatedAt, stored, source); err != nil {
		t.Fatalf("store the results row of acme/widgets#%d: %v", pull.number, err)
	}
	if pull.noTexts {
		return
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body,
			commit_messages, updated_at)
		values ('acme/widgets', $1, $2, $3, 'acme/widgets', $4, 'A change', 'No key here.', $5, $6)`,
		pull.number, pull.head, pull.branch, pull.base, pull.messages, notReadUpdatedAt); err != nil {
		t.Fatalf("store the texts of acme/widgets#%d: %v", pull.number, err)
	}
}

// storeTimelineOnly writes a timeline row of a merged acme/widgets#number under issueKey, and no
// results row. Dispatch then knows the pull request and its issue, but has not read its head.
func storeTimelineOnly(t *testing.T, database *store.Store, number int, issueKey string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into delivery_pull_requests (repo, number, title, url, author, merged_at, issue_key)
		values ('acme/widgets', $1, 'A change', $2, 'octocat', $3, $4)`,
		number, fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number), notReadUpdatedAt, issueKey); err != nil {
		t.Fatalf("store the timeline row of acme/widgets#%d: %v", number, err)
	}
}

// recordNotReadPass records a pass of blockID at commit on issue, and answers the id of the
// result.
func recordNotReadPass(t *testing.T, handler http.Handler, issue, blockID, commit string) string {
	t.Helper()
	response := recordResult(t, handler, issue, resultBody(blockID, "pass", commit, nil))
	if response.Code != http.StatusCreated {
		t.Fatalf("record a pass of %s at %s: status=%d body=%s", blockID, commit, response.Code, response.Body.String())
	}
	return decodeBody[recordedResult](t, response).Result.ID
}

// notReadListed is what delivery.Attributor.HeadsAttributedTo lists for issueKey: each number,
// with "?" after a number whose head Dispatch has not read. No server of this file holds a rename
// pair, so it reads with unrenamed.
func notReadListed(t *testing.T, database *store.Store, issueKey string) []string {
	t.Helper()
	heads, err := unrenamed().HeadsAttributedTo(context.Background(), database.Pool, issueKey, delivery.HeadAnswers{})
	if err != nil {
		t.Fatalf("HeadsAttributedTo(%s): %v", issueKey, err)
	}
	listed := []string{}
	for _, head := range heads {
		entry := "#" + strconv.Itoa(head.Number)
		if !head.HeadKnown {
			entry += "?"
		}
		listed = append(listed, entry)
	}
	return listed
}

// notReadAnswer is the answer of the statement for acme/widgets#number: the issue or "nobody",
// then "known" or "not known". It is "absent" where the statement has no answer. It reads with
// unrenamed, as notReadListed does.
func notReadAnswer(t *testing.T, database *store.Store, number int) string {
	t.Helper()
	key := delivery.PullRequestKey{Repo: "acme/widgets", Number: number}
	answers, err := unrenamed().PullRequestIssues(context.Background(), database.Pool, []delivery.PullRequestKey{key})
	if err != nil {
		t.Fatalf("PullRequestIssues(acme/widgets#%d): %v", number, err)
	}
	answer, found := answers[key]
	if !found {
		return "absent"
	}
	issue := "nobody"
	if answer.IssueKey != nil {
		issue = *answer.IssueKey
	}
	if !answer.Known {
		return issue + ", not known"
	}
	return issue + ", known"
}

// wantNotReadAnswer stops the test where the statement gives acme/widgets#number another
// answer than want. The fixture is then not the case that the test is about.
func wantNotReadAnswer(t *testing.T, database *store.Store, number int, want string) {
	t.Helper()
	if got := notReadAnswer(t, database, number); got != want {
		t.Fatalf("SETUP: the statement answers acme/widgets#%d %q, want %q", number, got, want)
	}
}

// wantNotReadListed stops the test where HeadsAttributedTo lists another set for issueKey.
func wantNotReadListed(t *testing.T, database *store.Store, issueKey string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if listed := notReadListed(t, database, issueKey); !slices.Equal(listed, want) {
		t.Fatalf("SETUP: HeadsAttributedTo(%s) lists %v, want %v", issueKey, listed, want)
	}
}

// describeStands is where one result stands, in one line: "<answer>/<reason>", then " at
// #<n>@<commit>" or " at branch <name>" for its basis, and " possible <repo>#<n>@<commit>" for
// its possible head. A nil stands is "none".
func describeStands(stands *model.ResultCommitStands) string {
	if stands == nil {
		return "none"
	}
	line := stands.Answer + "/" + stands.Reason
	if basis := stands.Basis; basis != nil {
		if basis.PullRequest != nil {
			line += fmt.Sprintf(" at #%d@%s", *basis.PullRequest, basis.Commit)
		} else {
			line += " at branch " + basis.Branch
		}
	}
	if head := stands.PossibleHead; head != nil {
		line += fmt.Sprintf(" possible %s#%d@%s", head.Repo, head.PullRequest, head.Commit)
	}
	return line
}

// comparedOf is each basis of check: "#<n>@<commit>" for a pull request, else "branch <name>".
// A pull request basis with no commit shows an empty commit.
func comparedOf(check model.CheckBlock) []string {
	compared := []string{}
	for _, basis := range check.ComparedWith {
		if basis.PullRequest != nil {
			compared = append(compared, fmt.Sprintf("#%d@%s", *basis.PullRequest, basis.Commit))
			continue
		}
		compared = append(compared, "branch "+basis.Branch)
	}
	return compared
}

// headsNotReadOf is each entry of the heads_not_read of check. An entry is its number, with
// refused_as and refused_since where they are set.
func headsNotReadOf(check model.CheckBlock) []string {
	listed := []string{}
	for _, head := range check.HeadsNotRead {
		entry := strconv.Itoa(head.Number)
		if head.RefusedAs != "" {
			entry += " refused_as=" + head.RefusedAs
		}
		if head.RefusedSince != nil {
			entry += " refused_since=" + head.RefusedSince.Format(time.RFC3339)
		}
		listed = append(listed, entry)
	}
	return listed
}

// issuesNotKnownOf is each number of the issues_not_known of check.
func issuesNotKnownOf(check model.CheckBlock) []int {
	numbers := []int{}
	for _, entry := range check.IssuesNotKnown {
		numbers = append(numbers, entry.Number)
	}
	return numbers
}

// wantNotReadLists fails the test where check lists other heads that Dispatch has not read, or
// other pull requests whose answer is not known. Each want is the list as fmt.Sprint prints it.
func wantNotReadLists(t *testing.T, check model.CheckBlock, headsNotRead, issuesNotKnown string) {
	t.Helper()
	if got := fmt.Sprint(headsNotReadOf(check)); got != headsNotRead {
		t.Errorf("check %s lists heads_not_read %s, want %s", check.BlockID, got, headsNotRead)
	}
	if got := fmt.Sprint(issuesNotKnownOf(check)); got != issuesNotKnown {
		t.Errorf("check %s lists issues_not_known %s, want %s", check.BlockID, got, issuesNotKnown)
	}
}

// wantNotReadCompared fails the test where check compares with other bases than compared, or
// names another reason for comparing with nothing.
func wantNotReadCompared(t *testing.T, check model.CheckBlock, compared, nothing string) {
	t.Helper()
	if got := fmt.Sprint(comparedOf(check)); got != compared || check.ComparedNothing != nothing {
		t.Errorf("check %s compares with %s for the reason %q, want %s for %q",
			check.BlockID, got, check.ComparedNothing, compared, nothing)
	}
}

// wantNotReadPassStands fails the test where check answers pass_stands other than want.
func wantNotReadPassStands(t *testing.T, check model.CheckBlock, want string) {
	t.Helper()
	if check.PassStands.Answer != want {
		t.Errorf("check %s answers pass_stands %q, want %q", check.BlockID, check.PassStands.Answer, want)
	}
}

// A pull request that Dispatch knows only from its timeline row has no head that Dispatch has
// read. The merged #81 of ATTR-1 is such a pull request. Check k1 has a pass at a commit that
// no basis names, and check k2 has no result. Each check compares with nothing for the reason
// head_not_read, and names #81 in heads_not_read. The pass is unknown for that reason, and
// pass_stands is unknown for each check, never no. The guard shows that HeadsAttributedTo lists
// #81 with no head, so the fixture is the case of the test.
func TestAPullRequestWhoseHeadIsNotReadIsNamedAndProvesNothing(t *testing.T) {
	t.Parallel()
	handler, database, _, _ := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "ATTR", "acme/widgets", "A head not read").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", notReadChecks)
	storeTimelineOnly(t, database, 81, issue)
	pass := recordNotReadPass(t, handler, issue, "k1", headCommit)
	wantNotReadListed(t, database, issue, "#81?")

	read := readResults(t, handler, issue)
	for _, blockID := range []string{"k1", "k2"} {
		check := checkOf(t, read, blockID)
		wantNotReadCompared(t, check, "[]", model.ReasonHeadNotRead)
		wantNotReadLists(t, check, "[81]", "[]")
		wantNotReadPassStands(t, check, model.PassStandsUnknown)
	}
	if got, want := describeStands(resultRead(t, read, "k1", pass).CommitStands), model.CommitUnknown+"/"+model.ReasonHeadNotRead; got != want {
		t.Errorf("the pass at a head that Dispatch has not read stands %s, want %s", got, want)
	}
}

// An issue with one pull request whose head Dispatch has read, and one whose head it has not
// read. The open #80 of ATTR-1 has a results row at notReadOwnHead. The merged #81 has a
// timeline row only. Each check compares with #80 and names #81 in heads_not_read. The pass of
// k1 at the head of #80 stands, so k1 answers yes. The only pass of k2 is at a commit that no
// head names. That pass is unknown with head_not_read, and k2 answers unknown, not no. The
// guard shows that HeadsAttributedTo lists #80 with its head and #81 with none.
func TestAPullRequestWhoseHeadIsNotReadBesideOneThatIs(t *testing.T) {
	t.Parallel()
	handler, database, _, _ := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "ATTR", "acme/widgets", "A head read and a head not read").Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", notReadChecks)
	storeNotReadPull(t, database, notReadPull{number: 80, head: notReadOwnHead, branch: "work/read",
		base: "main", state: "open", stored: issue, noTexts: true})
	storeTimelineOnly(t, database, 81, issue)
	own := recordNotReadPass(t, handler, issue, "k1", notReadOwnHead)
	other := recordNotReadPass(t, handler, issue, "k2", notReadOtherCommit)
	wantNotReadListed(t, database, issue, "#80", "#81?")

	read := readResults(t, handler, issue)
	for _, blockID := range []string{"k1", "k2"} {
		check := checkOf(t, read, blockID)
		wantNotReadCompared(t, check, "[#80@"+notReadOwnHead+"]", "")
		wantNotReadLists(t, check, "[81]", "[]")
	}
	k1, k2 := checkOf(t, read, "k1"), checkOf(t, read, "k2")
	if got, want := describeStands(resultRead(t, read, "k1", own).CommitStands), model.CommitAtBasis+"/ at #80@"+notReadOwnHead; got != want {
		t.Errorf("the pass at the head of #80 stands %s, want %s", got, want)
	}
	wantNotReadPassStands(t, k1, model.PassStandsYes)
	if got, want := describeStands(resultRead(t, read, "k2", other).CommitStands), model.CommitUnknown+"/"+model.ReasonHeadNotRead; got != want {
		t.Errorf("the pass at a commit that no read head names stands %s, want %s", got, want)
	}
	wantNotReadPassStands(t, k2, model.PassStandsUnknown)
}

// A pass at the head of a pull request whose answer is not known does not say no. Check k1 of
// NKWN-1 has a pass at headCommit. The merged #85 has headCommit as its head, and a current
// texts row that names no key. Dispatch has not read its commit messages, and its results row
// stores NKWN-1, as the sync or an older release stores it. The pass is unknown with
// issue_not_known, and its possible_head names #85 at headCommit. The check compares with
// nothing for issue_not_known, lists #85 in issues_not_known and names no head not read.
// pass_stands is unknown. The read is held off by a claim of headCommit
// (server.commitPulls.Claim), so it asks GitHub nothing there and the stored rows answer alone.
// Controls in the same test: with the message "Dispatch: NKWN-1" read, the pass stands at #85
// and pass_stands is yes. Then the pass is at newHeadCommit, and a known merged #84 of NKWN-1 is
// at a third commit. That pass is at another commit, and pass_stands is no. A guard reads the
// answer of the statement.
func TestAPassAtTheHeadOfAPullRequestWhoseAnswerIsNotKnownDoesNotSayNo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		messages string // "" is not read; %s is the issue of the check
		commit   string
	}{
		{"messages_not_read", "", headCommit},
		{"control_messages_read_naming_the_issue", "Work.\n\nDispatch: %s", headCommit},
		{"control_a_pass_at_another_commit_beside_a_known_head", "", newHeadCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
			issue := issueInRepository(t, handler, "NKWN", "acme/widgets", "Not known").Key
			uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
			pass := recordNotReadPass(t, handler, issue, "k1", tc.commit)
			var messages []string
			statement := "nobody, not known"
			if tc.messages != "" {
				messages = []string{fmt.Sprintf(tc.messages, issue)}
				statement = issue + ", known"
			}
			storeNotReadPull(t, database, notReadPull{number: 85, head: headCommit, branch: "work/no-key",
				base: "main", state: "merged", stored: issue, messages: messages})
			if tc.commit == newHeadCommit {
				storeNotReadPull(t, database, notReadPull{number: 84, head: notReadKnownHead, branch: "work/known",
					base: "main", state: "merged", stored: issue, noTexts: true})
			}
			wantNotReadAnswer(t, database, 85, statement)
			// GitHub names #85 for its head, so a read that asked would learn it again.
			fake.answers(func(repo, sha string) string {
				if sha != headCommit {
					return `[]`
				}
				return fmt.Sprintf(`[{"number":85,"title":"A change","body":"No key here.","html_url":"https://github.com/%s/pull/85",
					"state":"closed","merged":true,"merged_at":"2026-11-18T01:00:00Z","updated_at":"2026-11-18T01:00:00Z",
					"head":{"ref":"work/no-key","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`, repo, headCommit, repo)
			})
			if !served.commitPulls.Claim("acme/widgets", headCommit, time.Now()) {
				t.Fatal("SETUP: a claim of headCommit was refused; the record claims nothing")
			}
			spent := fake.asks("acme/widgets")
			read := readResults(t, handler, issue)
			asked := fake.asks("acme/widgets") - spent
			k1 := checkOf(t, read, "k1")
			stands := describeStands(resultRead(t, read, "k1", pass).CommitStands)
			switch tc.name {
			case "messages_not_read":
				want := fmt.Sprintf("%s/%s possible acme/widgets#85@%s", model.CommitUnknown, model.ReasonIssueNotKnown, headCommit)
				if stands != want {
					t.Errorf("the pass at the head of #85 stands %s, want %s", stands, want)
				}
				wantNotReadCompared(t, k1, "[]", model.ReasonIssueNotKnown)
				wantNotReadLists(t, k1, "[]", "[85]")
				wantNotReadPassStands(t, k1, model.PassStandsUnknown)
			case "control_messages_read_naming_the_issue":
				if want := model.CommitAtBasis + "/ at #85@" + headCommit; stands != want {
					t.Errorf("CONTROL: the pass at the head of #85 stands %s, want %s: the messages name the issue", stands, want)
				}
				wantNotReadPassStands(t, k1, model.PassStandsYes)
			default:
				if want := model.CommitOtherCommit + "/"; stands != want {
					t.Errorf("CONTROL: the pass at a commit that no pull request heads stands %s, want %s", stands, want)
				}
				wantNotReadPassStands(t, k1, model.PassStandsNo)
			}
			if tc.commit == headCommit && asked != 0 {
				t.Errorf("the read asked GitHub %d times, want none: the claim holds the read of headCommit off", asked)
			}
		})
	}
}

// A check that CI proves, beside a pull request whose answer is not known, does not say no. CI
// proves check c1 of NKCI-2. The open #85 has a current texts row that names no key, and
// Dispatch has not read its commit messages. Its results row stores NKCI-2. GitHub holds a
// successful run of the check at the head of #85. The CI answer is unread with the case
// issue_not_known, and its detail names #85. Dispatch reads no check run, and pass_stands is
// unknown. Control in the same test: the message that names NKCI-2 is read. Then the run at the
// head of #85 decides: the CI answer is pass, and pass_stands is yes.
func TestACheckThatCIProvesBesideAPullRequestWhoseAnswerIsNotKnownDoesNotSayNo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		messages string // "" is not read; %s is the issue of the check
	}{
		{"messages_not_read", ""},
		{"control_messages_read_naming_the_issue", "Work.\n\nDispatch: %s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, database, fake := resultsGitHubCheckRuns(t)
			issueInRepository(t, handler, "NKCI", "acme/widgets", "Not known")
			check := issueOfProject(t, handler, "NKCI", "Proved by CI", ciCheckBlock("c1", ciCheckKept))
			var messages []string
			statement := "nobody, not known"
			if tc.messages != "" {
				messages = []string{fmt.Sprintf(tc.messages, check)}
				statement = check + ", known"
			}
			storeNotReadPull(t, database, notReadPull{number: 85, head: headCommit, branch: "work/no-key",
				base: "main", state: "open", stored: check, messages: messages})
			wantNotReadAnswer(t, database, 85, statement)
			fake.set(headCommit, completedRun(85, ciCheckRun, "success"))

			c1 := checkOf(t, readResults(t, handler, check), "c1")
			state, ciCase, detail := "none", "", ""
			if c1.CI != nil {
				state, ciCase, detail = c1.CI.State, c1.CI.Case, c1.CI.Detail
			}
			switch tc.name {
			case "messages_not_read":
				wantNotReadPassStands(t, c1, model.PassStandsUnknown)
				if state != model.CIStateUnread || ciCase != "issue_not_known" {
					t.Errorf("ci %s (%s), want %s (issue_not_known)", state, ciCase, model.CIStateUnread)
				}
				want := fmt.Sprintf("Dispatch does not know yet whether pull request #85 is %s's: its commit messages are not read.", check)
				if detail != want {
					t.Errorf("the CI detail reads %q, want %q", detail, want)
				}
				if reads := fake.allReads(); reads != 0 {
					t.Errorf("Dispatch read %d pages of check runs, want none", reads)
				}
			default:
				if state != model.CIStatePass {
					t.Errorf("CONTROL: ci %s (%s), want %s: the messages name the issue", state, ciCase, model.CIStatePass)
				}
				wantNotReadPassStands(t, c1, model.PassStandsYes)
			}
		})
	}
}

// An `after_merge` check beside a merge whose answer is not known. The merged #70 of NKAM-1
// merged into release, and its branch names NKAM-1. Check k2 has a pass at integrationCommit,
// which only release carries. The merged #85 of the same repository merged into main, and its
// answer is not known. If #85 is the issue's, release does not decide. So the pass is unknown
// with issue_not_known, k2 lists #85 in issues_not_known, and pass_stands is unknown. The second
// subtest adds a pass at mergeCommit, which main carries: that pass stands at main, and
// pass_stands is yes. Control in the same test: without #85, the first pass stands at release.
// Guards read the merged branch of NKAM-1 and the answers of the statement.
func TestAnAfterMergeCheckBesideAMergeWhoseAnswerIsNotKnown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		notKnown bool // the merged #85, whose answer is not known, is stored
		onMain   bool // a second pass is at a commit that main carries
	}{
		{"a_pass_that_only_release_carries", true, false},
		{"and_a_pass_that_main_carries", true, true},
		{"control_no_merge_whose_answer_is_not_known", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
				switch {
				case base == "release" && head == integrationCommit:
					return "behind"
				case base == "main" && head == mergeCommit:
					return "behind"
				}
				return "diverged"
			})
			issue := issueInRepository(t, handler, "NKAM", "acme/widgets", "Merged into release").Key
			uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
			storeNotReadPull(t, database, notReadPull{number: 70, head: headCommit, branch: "legion/" + issue,
				base: "release", state: "merged", messages: []string{}})
			if tc.notKnown {
				storeNotReadPull(t, database, notReadPull{number: 85, head: newHeadCommit, branch: "work/no-key",
					base: "main", state: "merged"})
				wantNotReadAnswer(t, database, 85, "nobody, not known")
			}
			wantNotReadAnswer(t, database, 70, issue+", known")
			if merged, err := unrenamed().MergeOfIssue(context.Background(), database.Pool, issue, delivery.HeadAnswers{}); err != nil || len(merged) == 0 || merged[0].BaseRef != "release" {
				t.Fatalf("SETUP: MergeOfIssue(%s) answers %+v (%v), want its newest merge on release", issue, merged, err)
			}
			onRelease := recordNotReadPass(t, handler, issue, "k2", integrationCommit)
			onMain := ""
			if tc.onMain {
				onMain = recordNotReadPass(t, handler, issue, "k2", mergeCommit)
			}

			read := readResults(t, handler, issue)
			k2 := checkOf(t, read, "k2")
			wantNotReadCompared(t, k2, "[branch release branch main]", "")
			stands := describeStands(resultRead(t, read, "k2", onRelease).CommitStands)
			if !tc.notKnown {
				if want := model.CommitAtBasis + "/ at branch release"; stands != want {
					t.Errorf("CONTROL: the pass that only release carries stands %s, want %s", stands, want)
				}
				wantNotReadLists(t, k2, "[]", "[]")
				wantNotReadPassStands(t, k2, model.PassStandsYes)
				return
			}
			if want := model.CommitUnknown + "/" + model.ReasonIssueNotKnown; stands != want {
				t.Errorf("the pass that only release carries stands %s, want %s: #85 merged into main", stands, want)
			}
			wantNotReadLists(t, k2, "[]", "[85]")
			if !tc.onMain {
				wantNotReadPassStands(t, k2, model.PassStandsUnknown)
				return
			}
			if got, want := describeStands(resultRead(t, read, "k2", onMain).CommitStands), model.CommitAtBasis+"/ at branch main"; got != want {
				t.Errorf("the pass that main carries stands %s, want %s", got, want)
			}
			wantNotReadPassStands(t, k2, model.PassStandsYes)
		})
	}
}

// An issue with no head, beside a pull request whose answer is not known, is not known. NKNS-1
// has no pull request, and check k1 has a pass at newHeadCommit. The merged #86 of the same
// repository has a current texts row that names no key, and Dispatch has not read its commit
// messages. If #86 is the issue's, the issue has a head. So k1 compares with nothing for
// issue_not_known, lists #86 in issues_not_known, and pass_stands is unknown. The pass is at
// another commit. The second subtest does the same with the open #85. Control in the same test:
// with the messages of #86 read and naming no issue, k1 compares with nothing for
// no_pull_request_or_branch, and pass_stands is no. A guard reads the answer of the statement.
func TestAnIssueWithNoHeadBesideANotKnownPullRequestIsNotKnown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		state    string
		number   int
		messages []string // nil is not read
		nothing  string
		listed   string
		want     string
	}{
		{"no_head_and_a_merged_86_not_known", "merged", 86, nil, model.ReasonIssueNotKnown, "[86]", model.PassStandsUnknown},
		{"no_head_and_an_open_85_not_known", "open", 85, nil, model.ReasonIssueNotKnown, "[85]", model.PassStandsUnknown},
		{"control_86_known_as_nobodys", "merged", 86, []string{"Work with no key."}, model.ReasonNoPullRequestOrBranch, "[]", model.PassStandsNo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, database, _, _ := readThroughGitHub(t, "acme/widgets")
			issue := issueInRepository(t, handler, "NKNS", "acme/widgets", "No head").Key
			uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", twoChecks)
			pass := recordNotReadPass(t, handler, issue, "k1", newHeadCommit)
			storeNotReadPull(t, database, notReadPull{number: tc.number, head: headCommit, branch: "work/no-key",
				base: "main", state: tc.state, messages: tc.messages})
			statement := "nobody, not known"
			if tc.messages != nil {
				statement = "nobody, known"
			}
			wantNotReadAnswer(t, database, tc.number, statement)
			wantNotReadListed(t, database, issue)

			read := readResults(t, handler, issue)
			k1 := checkOf(t, read, "k1")
			wantNotReadCompared(t, k1, "[]", tc.nothing)
			wantNotReadLists(t, k1, "[]", tc.listed)
			wantNotReadPassStands(t, k1, tc.want)
			if got, want := describeStands(resultRead(t, read, "k1", pass).CommitStands), model.CommitOtherCommit+"/"; got != want {
				t.Errorf("the pass at a commit that no pull request heads stands %s, want %s", got, want)
			}
		})
	}
}

// Rule 2: a deployment with no App key, and a repository that the App cannot read, answer as
// the base answers. The rows are those of the first test of this file. Also, the merged #85 has
// the commit of the pass as its head, and its answer is not known. In each server, each check
// compares with nothing for repository_unreadable, and pass_stands is unknown for k1 and k2. The
// pass is unknown with repository_unreadable and no possible_head. Each check names #81 in
// heads_not_read and names no pull request whose answer is not known. The first server holds no
// App key. The App of the second server answers 404 for the installation on acme/widgets.
func TestNoAppKeyAndAnUnreadableRepositoryAnswerAsTheBaseDoes(t *testing.T) {
	t.Parallel()
	for _, server := range []struct {
		name  string
		serve func(t *testing.T) (http.Handler, *store.Store)
	}{
		{"no_app_key", func(t *testing.T) (http.Handler, *store.Store) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			if deps.GitHub != nil {
				t.Fatalf("SETUP: the fixture holds a GitHub client, want a deployment with no App key")
			}
			return handler, database
		}},
		{"app_not_installed_on_the_repository", func(t *testing.T) (http.Handler, *store.Store) {
			handler, database, _, _ := readThroughGitHub(t, "acme/other")
			return handler, database
		}},
	} {
		t.Run(server.name, func(t *testing.T) {
			handler, database := server.serve(t)
			issue := issueInRepository(t, handler, "ATTR", "acme/widgets", "A head not read, unreadable").Key
			uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md", notReadChecks)
			storeTimelineOnly(t, database, 81, issue)
			pass := recordNotReadPass(t, handler, issue, "k1", headCommit)
			storeNotReadPull(t, database, notReadPull{number: 85, head: headCommit, branch: "work/no-key",
				base: "main", state: "merged", stored: issue})
			wantNotReadAnswer(t, database, 85, "nobody, not known")
			wantNotReadListed(t, database, issue, "#81?")

			read := readResults(t, handler, issue)
			for _, blockID := range []string{"k1", "k2"} {
				check := checkOf(t, read, blockID)
				wantNotReadCompared(t, check, "[]", model.ReasonRepositoryUnreadable)
				wantNotReadLists(t, check, "[81]", "[]")
				wantNotReadPassStands(t, check, model.PassStandsUnknown)
			}
			if got, want := describeStands(resultRead(t, read, "k1", pass).CommitStands), model.CommitUnknown+"/"+model.ReasonRepositoryUnreadable; got != want {
				t.Errorf("the pass in a repository that Dispatch cannot read stands %s, want %s", got, want)
			}
		})
	}
}
