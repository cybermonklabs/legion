package workflow

import (
	"context"
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// refusalCode is the code of a fact's refusal, or "" when it was not refused.
func refusalCode(result intake.Result) string {
	if result.Refusal == nil {
		return ""
	}
	return result.Refusal.Code
}

// A review the review App submits decides a round only when the Legion footer it was written
// with names the pull request's current head, and a footer names a head only when it spells it
// as GitHub does - 40 lowercase hexadecimal characters (intake.PullRequestReview.LegionHead,
// refusesRound). So every fixture whose round a review decides seeds its heads from these, one
// per head of the branch, and writes its reviews with reviewBody.
const (
	// headEarlier is a head the branch left before headOne, for a fixture that needs a head no
	// round of it stands at.
	headEarlier = "9999999999999999999999999999999999999999"
	headOne     = "1111111111111111111111111111111111111111"
	headTwo     = "2222222222222222222222222222222222222222"
	headThree   = "3333333333333333333333333333333333333333"
)

// reviewBody is a review's body as a Legion role writes it: what the role said, then the Legion
// footer naming the session that wrote it and the head it read (prompts/roles/reviewer.md).
// The session it names is the one the shared fixtures record on the reviewer's claim
// (seedRoundOneRows), so it is the reviewer's own review, and a fixture about another session
// writes its own body. It names no entry, so it decides its round and moves no commit the reviewer
// read (record.PhaseRow.ReviewedHead): a fixture whose proof reads that commit writes its review
// with roundReviewBody.
func reviewBody(said, head string) string {
	return said + "\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\",\"head\":\"" + head + "\"} -->"
}

// reviewBodyOfEntry is a review's body whose Legion footer names session, head and entry as the
// entry of the round the review is of, the number the reviewer's task names (reviewround.EntryLabel),
// written as the reviewer's texts have an agent write it, a quoted number (`"entry":"<n>"`). A
// fixture about a review of the round that stands writes roundReviewBody, which reads that number;
// one about a review of another round names that round's here.
func reviewBodyOfEntry(said, session, head string, entry int64) string {
	return said + "\n\n<!-- legion: {\"session\":\"" + session + "\",\"phase\":\"review\",\"head\":\"" + head + "\",\"entry\":\"" + strconv.FormatInt(entry, 10) + "\"} -->"
}

// roundReviewBody is the body of session's review of the round that stands, as the reviewer
// writes it for the task it was given: its footer names the entry LEGION-208's reviewer row holds
// when the review is built (record.PhaseRow.Entry), the number that task names. It is the one
// helper of this package through which a fixture of the reviewer's own review ("ses-reviewer",
// the session seedRoundOneRows records on the reviewer's claim) names its entry, and through which
// a fixture of another session's review names the same entry, so that only the session tells the
// two apart. Build the review after the round's entry is raised.
func roundReviewBody(t *testing.T, pool *pgxpool.Pool, session, said, head string) string {
	t.Helper()
	return reviewBodyOfEntry(said, session, head, phaseRowOf(t, pool, claim.RoleReviewer).Entry)
}

// commentBody is reviewBody for the reviewer's COMMENT: its Legion footer names the reviewer's
// session and no head, since a comment reads no head the round could be decided on.
func commentBody(said string) string {
	return said + "\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\"} -->"
}

// reviewBodyOf is reviewBody for a review the review App submitted and what the author said for
// anyone else's: a person writes no Legion footer, and their review decides on the commit_id the
// webhook carried it with (refusesRound).
func reviewBodyOf(author, said, head string) string {
	if author == testReviewApp {
		return reviewBody(said, head)
	}
	return said
}

// mergingEngine is an engine of a gated project with a fixed clock, which the tests that drive an
// issue through its later phases share.
func mergingEngine() *Engine {
	return mergingEngineAt(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
}

// mergingEngineAt is mergingEngine with its clock at now.
func mergingEngineAt(now time.Time) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		// The project opens its production check with the tester, the role a project that names
		// no production_check gets (INFRA-809); an engine with no role opens no check at all.
		ProductionCheckRole: claim.RoleTester,
		Clock:               func() time.Time { return now },
	}, nil)
}

// applyFacts applies each fact to pool through engine, failing the test on an error, and returns
// the fact's result. The boot's open of each run no start counted comes before each fact
// (bootBefore), as it comes before any fact the daemon applies, and what the fact left is kept
// for the next pass's check (factLeft).
func applyFacts(t *testing.T, pool *pgxpool.Pool, engine *Engine) func(id string, fact intake.Fact) intake.Result {
	return func(id string, fact intake.Fact) intake.Result {
		t.Helper()
		bootBefore(t, pool, engine)
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", id, err)
		}
		factLeft(t, pool)
		return result
	}
}

// messageBodies is every Dispatch message the outbox holds, in order.
func messageBodies(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'body' from outbox where kind = 'dispatch_message' order by id")
	if err != nil {
		t.Fatalf("read Dispatch messages: %v", err)
	}
	defer rows.Close()
	bodies := []string{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan Dispatch message: %v", err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate Dispatch messages: %v", err)
	}
	return bodies
}

// readTask is a task as its reader reads it (texts.Render): the lines before its first block,
// each field's value by its key — a quoted value unquoted, a list's or a table's key with its
// [n] as written — each block of another author's words, in order, and round, the last line of a
// reviewer's task that holds the daemon's two sentences of its review round (reviewround.Ending),
// kept word for word and read as no field.
type readTask struct {
	lines  []string
	fields map[string]string
	blocks []taskBlock
	round  string
}

// reviewRoundLine is the last line of a reviewer's task naming commit, or `none`, and entry, or
// `none` where entry is not above 0 (reviewround.Ending).
func reviewRoundLine(commit string, entry int64) string {
	return strings.TrimPrefix(reviewround.Sentences(commit, entry), " ")
}

// taskBlock is one block of a task: its tag, the field it is named by (the tag less a -N), its
// attributes and the words between its tags.
type taskBlock struct {
	tag, name string
	attrs     map[string]string
	text      string
}

var (
	blockOpening   = regexp.MustCompile(`^<([a-z_]+)(-[0-9]+)?((?: [a-z]+="[^"]*")*)>$`)
	blockAttribute = regexp.MustCompile(` ([a-z]+)="([^"]*)"`)
)

// readTaskOf reads task, failing the test where a block is not closed on a line of its own or
// anything but a block follows the first block. It logs the task, so each test prints the tasks
// it reads.
func readTaskOf(t *testing.T, task string) readTask {
	t.Helper()
	t.Logf("task:\n%s", task)
	read := readTask{fields: map[string]string{}}
	lines := strings.Split(task, "\n")
	if last := lines[len(lines)-1]; len(lines) > 1 && strings.HasPrefix(last, reviewround.CommitLabel+": ") {
		read.round, lines = last, lines[:len(lines)-1]
	}
	i := 0
	for ; i < len(lines) && !blockOpening.MatchString(lines[i]); i++ {
		read.lines = append(read.lines, lines[i])
		key, value, ok := strings.Cut(lines[i], ": ")
		if !ok || strings.HasPrefix(lines[i], " ") {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil && strings.HasPrefix(value, `"`) {
			value = unquoted
		}
		read.fields[key] = value
	}
	for i < len(lines) {
		opening := blockOpening.FindStringSubmatch(lines[i])
		if opening == nil {
			t.Fatalf("the task's line %d, %q, follows a block and opens none:\n%s", i+1, lines[i], task)
		}
		block := taskBlock{tag: opening[1] + opening[2], name: opening[1], attrs: map[string]string{}}
		for _, attribute := range blockAttribute.FindAllStringSubmatch(opening[3], -1) {
			block.attrs[attribute[1]] = html.UnescapeString(attribute[2])
		}
		end := i + 1
		for end < len(lines) && lines[end] != "</"+block.tag+">" {
			end++
		}
		if end == len(lines) {
			t.Fatalf("the task's block %s, opened at line %d, is never closed:\n%s", block.tag, i+1, task)
		}
		block.text = strings.Join(lines[i+1:end], "\n")
		read.blocks = append(read.blocks, block)
		i = end + 1
	}
	return read
}

// block is the one block of r named name, failing the test unless there is exactly one.
func (r readTask) block(t *testing.T, name string) taskBlock {
	t.Helper()
	var found []taskBlock
	for _, block := range r.blocks {
		if block.name == name {
			found = append(found, block)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the task has %d blocks named %s (%+v), want one", len(found), name, r.blocks)
	}
	return found[0]
}

// hasLines says whether task holds each of lines as a line of its own: a field as the task's
// encoder writes it, such as "phase: retro".
func hasLines(task string, lines ...string) bool {
	held := strings.Split(task, "\n")
	for _, line := range lines {
		if !slices.Contains(held, line) {
			return false
		}
	}
	return true
}
