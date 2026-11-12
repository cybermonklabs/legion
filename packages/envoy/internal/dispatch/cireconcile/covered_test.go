package cireconcile

import (
	"reflect"
	"testing"
	"time"
)

// unrenamedRow is a mapping that was never renamed and sorts after the renamed pair, so a read
// that answered one repository for the whole table would drop it rather than the pair.
const unrenamedRow = "zed/other"

// Every mapped repository is read, whatever any other mapping was renamed to: a pass over a
// renamed pair and a mapping of its own reads two repositories, the pair contributing the name
// it has today once. A rename is one mapping's fact, so a reader that let one mapping's
// renamed_to stand for every row would leave every repository but that one out of the pass —
// no head read, no record mended, no settlement — for as long as any rename is recorded.
func TestEveryMappingIsReadWhenAnotherNamesARename(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRenamedRepository(renamedAwayRow, rowRepository)
	h.mapRepository(unrenamedRow)

	repositories, err := h.reconciler.covered(h.ctx)
	if err != nil {
		t.Fatalf("read the mapped repositories: %v", err)
	}

	want := []string{rowRepository, unrenamedRow}
	if !reflect.DeepEqual(repositories, want) {
		t.Fatalf("the pass reads %v, want %v: each mapped repository under the name it has now, once", repositories, want)
	}
}

// The three mappings the test below reads, whose names are the measured pair of the defect with
// one more after both: `owner/legionabc` sorts before `owner/legion-x` under a collation that
// weighs punctuation last, which is glibc's en_US, while Go, which compares the bytes, puts
// `owner/legion-x` first. `owner/zed` sorts after both under either rule, so it is the
// repository a seek that has run too far still finds.
const (
	punctuatedRow        = "owner/legion-x"
	punctuatedRepository = "Owner/Legion-X"
	punctuatedID         = 5101

	letteredRow        = "owner/legionabc"
	letteredRepository = "Owner/Legionabc"
	letteredID         = 5102

	lastMappedRow        = "owner/zed"
	lastMappedRepository = "Owner/Zed"
	lastMappedID         = 5103
)

// collateMappingsAsADeploymentDoes holds the mapping's names under a collation that sorts
// `owner/legionabc` before `owner/legion-x`, which is what a deployment's database does: its
// collation is the one the cluster's Postgres was created with, glibc's en_US on the measured
// one. The default collation of the database a test runs against is whatever its server was
// created with (ICU en-US on the pod's test server, and other servers may differ), so the test
// cannot rely on it to tell the two rules apart; it puts the names under one known collation of
// its own.
func (h *harness) collateMappingsAsADeploymentDoes() {
	h.t.Helper()
	// A PostgreSQL built with ICU carries ICU collations of its own and one built without ICU
	// carries none, so this count is the server's own answer to whether the collation below can
	// be made. A server that will not answer the question is not a server without ICU, so an
	// error from the read itself ends the test.
	var icu bool
	if err := h.pool.QueryRow(h.ctx, `select count(*) > 0 from pg_collation where collprovider = 'i'`).Scan(&icu); err != nil {
		h.t.Fatalf("ask whether this Postgres was built with ICU: %v", err)
	}
	if !icu {
		h.t.Skip("a PostgreSQL built with ICU must serve these tests: ICU is what holds the mapping under one known collation weighing punctuation last, an order the Go comparison that seeks into it does not follow")
	}
	// ICU's `ka-shifted` weighs punctuation only where the letters are equal, which is the rule
	// glibc's en_US was measured to follow. Both name columns take it: the canonical name of a
	// row is a coalesce of the two (refs.CanonicalRepoSQL), and Postgres derives no collation at
	// all for a coalesce whose arms carry different ones.
	for _, statement := range []string{
		`create collation punctuation_last (provider = icu, locale = 'en-US-u-ka-shifted')`,
		`alter table repo_projects
			alter column repo type text collate punctuation_last,
			alter column renamed_to type text collate punctuation_last`,
	} {
		// The server has ICU, so a statement it refuses here is a permission or a schema
		// problem rather than an absence to pass over, and the test says so rather than skips.
		if _, err := h.pool.Exec(h.ctx, statement); err != nil {
			h.t.Fatalf("hold the mapping under a deployment's own collation (%s): %v", statement, err)
		}
	}
}

// Every covered repository is read once a cycle however the database collates the mapping's
// names. The order the pass walks is covered()'s, and the pass taking a cursor up seeks
// into that order with Go's own comparison of the names, so the two are one rule or the seek is
// reading an order it does not understand.
//
// A pass is cut short in `owner/legionabc` and that repository is then unmapped, which is the
// case where it tells: the seek is for a name no row carries, and under the database's own rule
// it runs past `owner/legion-x`, which that rule puts after the cursor and Go puts before it.
// No pass of the cycle then reads `owner/legion-x` -- the pass before the cursor had not
// reached it, the pass after the cursor seeks past it -- and the pass that reaches the end of
// the order records that the cycle covered it.
func TestEveryCoveredRepositoryIsReadHoweverTheDatabaseCollatesTheNames(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.collateMappingsAsADeploymentDoes()
	mapped := []struct {
		row, repository string
		id              int64
		heads           []fakePull
	}{
		{punctuatedRow, punctuatedRepository, punctuatedID, []fakePull{head(1)}},
		// The repository the pass is cut short in has a second head to be cut short at.
		{letteredRow, letteredRepository, letteredID, []fakePull{head(1), head(2)}},
		{lastMappedRow, lastMappedRepository, lastMappedID, []fakePull{head(1)}},
	}
	for _, repository := range mapped {
		h.mapRepository(repository.row)
		h.github.setRepository(repository.row, repository.repository, repository.id)
		h.github.setPullRequests(repository.repository, repository.heads...)
		for _, pull := range repository.heads {
			h.github.setCheckRuns(repository.repository, pull.sha,
				atHead(repository.repository, pull.sha, checkRun(repository.id+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
			)
		}
	}
	// GitHub answers the second head of `owner/legionabc` no checks to the query, so the pass
	// reads that head over REST, and is slower with it than the pass has left: the pass ends
	// inside that repository and leaves the cursor there.
	cutAt := head(2)
	h.github.refuseChecks(letteredRepository, cutAt.sha)
	h.github.stall("/repos/"+letteredRepository+"/commits/"+cutAt.sha+"/check-runs", time.Minute)
	h.reconciler.PassTimeout = 3 * time.Second

	h.pass()

	first := h.passLine()
	if first["deadline_reached"] != true {
		t.Fatalf("the first pass reports deadline_reached = %v, want true: GitHub held a head of %s past its bound: %v",
			first["deadline_reached"], letteredRow, first)
	}

	// The repository the cursor names leaves the mapping, as an operator unmapping it does, so
	// the pass after it seeks for a name the order no longer holds.
	if _, err := h.pool.Exec(h.ctx, `delete from repo_projects where repo = $1`, letteredRow); err != nil {
		t.Fatalf("unmap %s: %v", letteredRow, err)
	}
	h.github.stall("/repos/"+letteredRepository+"/commits/"+cutAt.sha+"/check-runs", 0)
	h.reconciler.PassTimeout = 0

	h.pass()

	line := h.passLine()
	if line["cycle_complete"] != true {
		t.Fatalf("the second pass reports cycle_complete = %v, want true: it read the order out to its last repository: %v", line["cycle_complete"], line)
	}
	if state := h.stateAt(punctuatedRepository, 1, head(1).sha); state == nil {
		t.Errorf("the head of %s has no record: no pass of the cycle read that repository, and the pass that closed the cycle recorded that it had covered it. The mapped order and the seek into it put it on different sides of the cursor",
			punctuatedRow)
	}
	if state := h.stateAt(lastMappedRepository, 1, head(1).sha); state == nil {
		t.Errorf("the head of %s has no record: the pass after the cursor read none of the repositories the cursor's own sorts before", lastMappedRow)
	}
}

// A repository an issue names and no project maps is reconciled like any other, so a head there
// settles. An issue's own repository is any `owner/name`, and work on such an issue opens pull
// requests whose heads the listener records and whose settlements the Legion daemon waits for;
// a repair that read repo_projects alone would never read those heads, so a webhook GitHub
// never delivered there would hold the head short of a check for ever -- and silently, since
// nothing ever asked GitHub about that repository to skip it.
func TestAHeadSettlesInARepositoryOnlyAnIssueNames(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.issueInRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-12T04:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-12T04:01:20Z"),
	)
	var mapped int
	if err := h.pool.QueryRow(h.ctx, `select count(*) from repo_projects`).Scan(&mapped); err != nil {
		t.Fatalf("count the repository mappings: %v", err)
	}
	if mapped != 0 {
		t.Fatalf("repo_projects holds %d rows, want none: this test is about a repository no project maps", mapped)
	}
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after a pass over the repository the issue names")
	if settled.Number != pullNumber || settled.SHA != headSHA {
		t.Fatalf("the settlement is of %s at %s, want %s at %s", settled.Number, settled.SHA, pullNumber, headSHA)
	}
	if settled.Failed.Count != 0 || settled.Passed.Count != 1 || len(settled.Passed.Checks) != 1 || settled.Passed.Checks[0] != "ci-ok" {
		t.Fatalf("settlement = %+v, want ci-ok alone passing", settled)
	}
}
