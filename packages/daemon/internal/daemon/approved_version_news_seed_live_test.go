//go:build e2e

package daemon

// approved_version_news_seed_live_test.go holds part one's seed of the approved-version-news live
// proof and its setup checks: the seed that starts each claim through the daemon's own start path,
// and the checks that every claim stands at the entry that path gives it. Part two's seed
// (approved_version_news_sessions_live_test.go) starts its roles through the same path and checks
// them by the same rule, so the pieces both seeds use are here: the start of a fixture's roles
// (startRoles), the rule a claim's entries are held to (claimEntries, entryRefusal), the read of a
// told row, in the seed's transaction or in a savepoint of it (heldTold, toldInSavepoint), and the
// write of a told row by hand, checked against the row the start path writes (seedToldRow).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// awaitStartTaken waits until the task c's start queued has reached c and its turn has taken it:
// c was prompted, and its claim holds no task. Every start the seed writes carries a task: an
// architect's carries a fresh tree's first act, and a worker's its phase's resume task. So no step
// begins with a task of the boot still to arrive, and no window a step watches for silence holds
// one.
func (c *versionNewsClaimant) awaitStartTaken() {
	c.r.t.Helper()
	c.r.await("the task the start of "+string(c.token)+" queued, sent and taken", versionNewsWait, func() (bool, string) {
		prompts := len(c.since(time.Time{}))
		pending := c.r.d.claim(c.token).Pending
		if prompts == 0 || pending != nil {
			held := "no task"
			if pending != nil {
				held = "task " + pending.ID
			}
			return false, fmt.Sprintf("%d prompts, its claim holding %s", prompts, held)
		}
		return true, ""
	})
}

// standAtTheirEntries is the boot's SETUP check that the start path left each claim it brought to
// ready where production's claims stand (standingEntry). A claim that stands anywhere else fails
// the run in setup, so a change that took the fixture back to entry 0, where no step drives the
// comparison of the two entries, fails here rather than passing every step. Each tree's claims
// are noted with their entries, which is the run's evidence of them.
func (r *versionNewsLive) standAtTheirEntries() {
	r.t.Helper()
	for _, name := range slices.Sorted(maps.Keys(r.trees)) {
		tree := r.trees[name]
		var stood []string
		for _, member := range tree.members {
			for _, role := range member.roles {
				stood = append(stood, fmt.Sprintf("the %s of %s at entry %d", role, member.key, r.standingEntry(r.claimant(member, role))))
			}
		}
		r.note("tree %s as the daemon's start path left it, each claim given its entry: %s", name, strings.Join(stood, "; "))
	}
}

// standingEntry is the entry c stands at, and fails the run unless c stands where the daemon's
// start path leaves every claim it starts: at an entry of 1 or more of its role's phases row, given
// that entry. Whether a holder takes its news directly compares those two entries
// (newsClaimTakes), and a claim at entry 0 drives no such comparison. The FAIL line names the claim
// and both numbers.
func (r *versionNewsLive) standingEntry(c *versionNewsClaimant) int64 {
	r.t.Helper()
	entry, given, err := claimEntries(context.Background(), r.st.Pool(), c.issue, c.role, c.token)
	if err != nil {
		r.fail("read the entries of %s: %v", c.name(), err)
	}
	if refusal := entryRefusal(c.name(), entry, given); refusal != "" {
		r.fail("%s", refusal)
	}
	return entry
}

// claimEntries reads the entry role's phases row on issue names and the entry token's claim was
// given, each 0 where its row does not exist: the two numbers whose comparison decides whether a
// holder takes its news directly (newsClaimTakes).
func claimEntries(ctx context.Context, db record.RowQuerier, issue string, role claim.Role, token claim.Token) (entry, given int64, err error) {
	err = db.QueryRow(ctx, `select coalesce((select entry from phases where issue = $1 and role = $2), 0),
		coalesce((select given_entry from claims where token = $3), 0)`, issue, string(role), string(token)).Scan(&entry, &given)
	return entry, given, err
}

// entryRefusal is why a claim, named name, that stands at entry of its role's phases row and was
// given given does not stand where the daemon's start path leaves every claim it starts: at an
// entry of 1 or more, given that entry. It is "" where the claim stands there. Both seeds fail a
// run in setup on it, and the sentence names the claim and both numbers.
func entryRefusal(name string, entry, given int64) string {
	if entry >= 1 && given == entry {
		return ""
	}
	return fmt.Sprintf("%s stands at entry %d of its phases row and was given entry %d, where the daemon's start path leaves each claim it starts "+
		"at an entry of 1 or more, given that entry", name, entry, given)
}

// seed records every member of every tree as the daemon holds it mid-run, and starts each claim
// the fixture names through the daemon's own start path (seedTree), in one transaction under a
// commit scope (intake.WithCommitHooks), as a fact the daemon applies writes them. Each member is
// recorded in its tree, in its phase, holding an admission slot, on an open pull request unless it
// stands in admitted, and its tree's lifecycle is opened under the workflow. Every role's prompt
// is on disk, as the daemon's own launch writes it, so a launch the run makes later starts too.
// Where the schema holds the told record, each member in a phase with work takes a holder row
// written by hand and checked against the row the start path writes for it (seedHolderRow): a
// difference fails the run in setup, and the FAIL line names the issue, the field and both values.
func (r *versionNewsLive) seed() {
	t := r.t
	t.Helper()
	ctx, committed := intake.WithCommitHooks(context.Background())
	number, err := strconv.ParseInt(randomSuffix(t)[:5], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	var told bool
	if err := r.st.Pool().QueryRow(ctx, "select to_regclass('design_news_told') is not null").Scan(&told); err != nil {
		t.Fatalf("read whether the schema holds the told record: %v", err)
	}
	// The engine the booted daemon runs, built from the same configuration (engineConfig), so
	// each start written through it is the row the daemon's own admission writes.
	engine := workflow.New(record.NewStore(), engineConfig(r.cfg, "", nil), quietLogger())
	names := slices.Sorted(maps.Keys(r.trees))
	var differences []string
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		differences = nil
		for _, name := range names {
			differs, err := r.seedTree(ctx, tx, engine, r.trees[name], told, &number)
			if err != nil {
				return err
			}
			differences = append(differences, differs...)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the trees through the daemon's start path: %v", err)
	}
	committed()
	if len(differences) > 0 {
		r.fail("%s", strings.Join(differences, "; "))
	}
	for _, name := range names {
		tree := r.trees[name]
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, tree.root().key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", tree.root().key, err)
		}
		for _, member := range tree.members {
			for _, role := range claim.Roles {
				writePrompt(t, r.cfg, r.token(member.key, role))
			}
		}
		if told && slices.ContainsFunc(tree.members, func(member *versionNewsIssue) bool { return member.phase != phase.Admitted }) {
			r.note("tree %s: each member in a phase with work took %s, in a row written by hand and the same as the row the daemon's start path writes "+
				"for a holder while its tree's gate stands approved at version 1", name, toldVersions(tree))
		}
	}
	if !told {
		r.note("the schema holds no design_news_told table, so no told record is seeded: this daemon keeps none")
	}
}

// seedTree records tree's members and starts each claim the fixture names on them through the
// daemon's start path (startRoles), in tx, and answers how each holder row written by hand differs
// from the row that path writes (seedHolderRow), one entry for each row that differs. Each member's
// holder row is written before its own start, whose raise leaves a row already there as it stands.
func (r *versionNewsLive) seedTree(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, tree *versionNewsTree, told bool, number *int64) ([]string, error) {
	issues := make([]record.Issue, len(tree.members))
	for i, member := range tree.members {
		var listed struct {
			Rank    string `json:"rank"`
			LastSeq int64  `json:"last_seq"`
		}
		r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+member.key, nil, http.StatusOK, &listed)
		issue := record.Issue{
			Key: member.key, Repo: r.repo, DefaultBranch: "main", GitHubRepo: r.repo, Tree: tree.root().key, Project: r.cfg.Project,
			Title: "Approved version news " + tree.name, Phase: member.phase, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
		}
		if member.parent != "" {
			parent := member.parent
			issue.Parent = &parent
		}
		issues[i] = issue
		if err := r.records.PutIssue(ctx, tx, issue); err != nil {
			return nil, err
		}
		if member.phase != phase.Admitted {
			*number++
			member.pr = int(*number)
			if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
				State: record.PullRequestOpen, Issue: member.key, Repo: r.repo, Number: member.pr, Branch: "legion/" + member.key,
				HeadSHA: versionNewsHead(member.key), Base: "main", Failing: []string{},
			}); err != nil {
				return nil, err
			}
		}
		if err := r.admit(ctx, tx, member.key); err != nil {
			return nil, err
		}
	}
	root := issues[0]
	var differences []string
	for i, member := range tree.members {
		if told && member.phase != phase.Admitted {
			differs, err := r.seedHolderRow(ctx, tx, engine, tree, root, issues[i])
			if err != nil {
				return nil, err
			}
			if differs != "" {
				differences = append(differences, differs)
			}
		}
		if err := startRoles(ctx, tx, r.records, engine, issues[i], member.roles); err != nil {
			return nil, fmt.Errorf("tree %s: %w", tree.name, err)
		}
	}
	return differences, nil
}

// startRoles starts each of roles on issue through the daemon's own start path, in tx, as
// admission's promotion starts them. A tree root's architect is started as the promotion of a
// queued root starts it (workflow.Engine.StartArchitect), on the root recorded at admitted, where
// that promotion finds it: the derivation gives a root at admitted a first task, raised as the
// architect's entry. The root is then recorded again in the phase it stands in, as the transitions
// after that promotion leave it. A child's own architect is started as the child's first phase
// start starts it (workflow.Engine.StartFirstPhase), on the child recorded at admitted, where the
// waiting line finds it once the tree's gate is open: that start queues the child's plain architect
// start, which hands the role no entry and no task, beside the rows of the child's move into
// planning. The child is then recorded again in the phase it stands in. A member's phase worker is
// started as the promotion of a mid-phase member starts it (workflow.Engine.StartPromotedWorker),
// which raises that role's entry on the phase the member stands in, with the phase's resume task
// as the entry's work. Each start queues its role's start on the outbox, and each but the child
// architect's writes its role's phases row. A role that is none of these has no start on that
// path, and is refused.
func startRoles(ctx context.Context, tx pgx.Tx, records record.Store, engine *workflow.Engine, issue record.Issue, roles []claim.Role) error {
	for _, role := range roles {
		switch {
		case role == claim.RoleArchitect:
			admitted := issue
			admitted.Phase = phase.Admitted
			if err := records.PutIssue(ctx, tx, admitted); err != nil {
				return err
			}
			if claim.IsTreeRoot(issue.Key, issue.Tree) {
				if err := engine.StartArchitect(ctx, tx, admitted); err != nil {
					return fmt.Errorf("start the architect of %s: %w", issue.Key, err)
				}
			} else if err := engine.StartFirstPhase(ctx, tx, admitted, nil); err != nil {
				return fmt.Errorf("start the first phase of %s, which starts its own architect: %w", issue.Key, err)
			}
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		case role == phase.RoleFor(issue.Phase):
			if err := engine.StartPromotedWorker(ctx, tx, issue, role); err != nil {
				return fmt.Errorf("start the %s of %s: %w", role, issue.Key, err)
			}
		default:
			return fmt.Errorf("the fixture names the %s of %s in %s, which the daemon's start path does not start: it starts a root's "+
				"architect, a child's own architect and a member's phase worker", role, issue.Key, issue.Phase)
		}
	}
	return nil
}

// toldVersions is the versions a holder row of tree's told record names when its holder took
// version 1 of each document tree registers, as the row encodes them.
func toldVersions(tree *versionNewsTree) string {
	versions := map[string]int{}
	for _, step := range tree.steps() {
		versions[string(step)] = 1
	}
	encoded, err := json.Marshal(versions)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// versionNewsTold is a row of the told record by the three fields its writers set: versions as the
// database prints them, whether it owes the opening, and its opening's number. versions reads "no
// row" where there is none.
type versionNewsTold struct {
	versions    string
	openingOwed bool
	openingSeq  int64
}

// heldTold is issue's row of the told record for audience ("holder" or "architect") as tx reads
// it. The audience is the row's own text rather than the record package's name for it, so part
// one's driver still builds against a base commit, whose record package names no audience.
func heldTold(ctx context.Context, tx pgx.Tx, issue, audience string) (versionNewsTold, error) {
	var told versionNewsTold
	err := tx.QueryRow(ctx, `select versions::text, opening_owed, opening_seq from design_news_told where issue = $1 and audience = $2`,
		issue, audience).Scan(&told.versions, &told.openingOwed, &told.openingSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return versionNewsTold{versions: "no row"}, nil
	}
	if err != nil {
		return versionNewsTold{}, fmt.Errorf("read the %s row of %s: %w", audience, issue, err)
	}
	return told, nil
}

// toldInSavepoint answers the told row read reads in a savepoint of tx, which is rolled back once
// read has answered, so nothing written to answer it stays. what names the row, for an error. The
// rows a seed writes by hand are checked against the rows the start path writes in such a
// savepoint, for a state the seed itself cannot write.
func toldInSavepoint(ctx context.Context, tx pgx.Tx, what string, read func(pgx.Tx) (versionNewsTold, error)) (versionNewsTold, error) {
	probe, err := tx.Begin(ctx)
	if err != nil {
		return versionNewsTold{}, fmt.Errorf("open the savepoint %s is read in: %w", what, err)
	}
	told, err := read(probe)
	if rollback := probe.Rollback(ctx); rollback != nil && err == nil {
		err = fmt.Errorf("roll back the savepoint %s was read in: %w", what, rollback)
	}
	return told, err
}

// differences is each field of seeded that differs from path, named with both values.
func (seeded versionNewsTold) differences(path versionNewsTold) []string {
	var differs []string
	if seeded.versions != path.versions {
		differs = append(differs, fmt.Sprintf("versions %s as seeded and %s from the start path", seeded.versions, path.versions))
	}
	if seeded.openingOwed != path.openingOwed {
		differs = append(differs, fmt.Sprintf("opening_owed %t as seeded and %t from the start path", seeded.openingOwed, path.openingOwed))
	}
	if seeded.openingSeq != path.openingSeq {
		differs = append(differs, fmt.Sprintf("opening_seq %d as seeded and %d from the start path", seeded.openingSeq, path.openingSeq))
	}
	return differs
}

// seedToldRow writes issue's row of the told record for audience by hand, holding versions and
// owing no opening, and answers how it differs from path, the row the daemon's start path writes
// for the same audience (versionNewsTold.differences), none where it does not. what names the row,
// for an error.
func seedToldRow(ctx context.Context, tx pgx.Tx, issue, audience, versions, what string, path versionNewsTold) ([]string, error) {
	if _, err := tx.Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
		values ($1, $2, $3::jsonb, false, now())`, issue, audience, versions); err != nil {
		return nil, fmt.Errorf("seed %s: %w", what, err)
	}
	seeded, err := heldTold(ctx, tx, issue, audience)
	if err != nil {
		return nil, err
	}
	return seeded.differences(path), nil
}

// seedHolderRow writes member's holder row of the told record by hand, at version 1 of each
// document tree registers (toldVersions), and answers how it differs from the row the daemon's
// start path writes for the same holder, "" where it does not. member's own start comes after it
// (seedTree), and its raise leaves a row already there as it stands.
//
// The start path's row is read in a savepoint rolled back once the row is read
// (toldByStartPath), because the state it is written for cannot be seeded: the tree's gate
// approved at version 1 of each of those documents, which each step opens itself after the boot.
func (r *versionNewsLive) seedHolderRow(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, tree *versionNewsTree, root, member record.Issue) (string, error) {
	path, err := r.toldByStartPath(ctx, tx, engine, tree, root, member)
	if err != nil {
		return "", err
	}
	differs, err := seedToldRow(ctx, tx, member.Key, "holder", toldVersions(tree), "the holder row of "+member.Key, path)
	if err != nil {
		return "", err
	}
	if len(differs) == 0 {
		return "", nil
	}
	return fmt.Sprintf("tree %s, the holder row of %s: %s, where the start path writes the row for a holder while its tree's gate stands "+
		"approved at version 1", tree.name, member.Key, strings.Join(differs, ", ")), nil
}

// toldByStartPath is the holder row the daemon's start path writes for member while tree's gate
// stands approved at version 1 of each document tree registers, read in a savepoint of tx that is
// rolled back once the row is read (toldInSavepoint), so nothing of it stays. In the savepoint it
// writes the gate rows a person's approval of each of those versions leaves on root, then raises a
// worker's entry on member (workflow's Engine.enter, through Engine.StartPromotedWorker), which
// writes the row from the gate as it stands. The worker is the planner, the holder of every
// issue's first working phase: its raise writes the row, and each later holder of the issue
// inherits it.
func (r *versionNewsLive) toldByStartPath(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, tree *versionNewsTree, root, member record.Issue) (versionNewsTold, error) {
	return toldInSavepoint(ctx, tx, "the start path's holder row of "+member.Key, func(probe pgx.Tx) (versionNewsTold, error) {
		approved := 1
		for _, step := range tree.steps() {
			artifact := tree.spec
			if step == record.DesignStepCriteria {
				artifact = tree.verification
			}
			if err := r.records.PutGate(ctx, probe, record.DesignGate{Issue: root.Key, Step: step, ArtifactID: artifact, LatestVersion: 1,
				ApprovedVersion: &approved}); err != nil {
				return versionNewsTold{}, err
			}
		}
		planning := member
		planning.Phase = phase.Planning
		if err := engine.StartPromotedWorker(ctx, probe, planning, claim.RolePlanner); err != nil {
			return versionNewsTold{}, fmt.Errorf("raise the planner's entry on %s: %w", member.Key, err)
		}
		return heldTold(ctx, probe, member.Key, "holder")
	})
}

// versionNewsHead is the head of key's pull request.
func versionNewsHead(key string) string { return "head-" + strings.ToLower(key) }
