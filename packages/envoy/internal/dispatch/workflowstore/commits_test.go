package workflowstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The commits of a run: the commit its task's head was at when it started and when it ended, and
// for a fix, a run that fixes a CI check failed at that head, the block id of that check. The
// daemon writes them onto the run's record beside its start and its end, in any order with them.

// Three commits of one task's head, in the order the head moved.
const (
	commitX = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	commitY = "b2c3d4e5f60718293a4b5c6d7e8f9012345678a1"
	commitZ = "c3d4e5f60718293a4b5c6d7e8f9012345678a1b2"
)

// commitsOf is the write of a run's commits and fix check: its identity and each of the three that
// is not "".
func commitsOf(identity StepWrite, start, end, check string) StepWrite {
	for _, field := range []struct {
		value  string
		target **string
	}{{start, &identity.StartCommit}, {end, &identity.EndCommit}, {check, &identity.FixCheck}} {
		if field.value != "" {
			*field.target = new(field.value)
		}
	}
	return identity
}

// tuplesOf counts the tuples of the log whose kind is one of kinds.
func tuplesOf(t *testing.T, pool *store.Pool, kinds ...string) int {
	t.Helper()
	count := 0
	for _, tuple := range wholeLog(t, pool) {
		for _, kind := range kinds {
			if tuple.kind == kind {
				count++
			}
		}
	}
	return count
}

// A run's record takes the commit it started at and the commit it ended at, each once: the start
// at X and the end at Y land beside the run's own start and end, and a second end at Z changes
// nothing, appends nothing, and the read lists X and Y.
func TestAStepRecordTakesItsCommitsOnce(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	started := put(t, pool, commitsOf(implementer(id), commitX, "", ""), minute(0))
	if started.StartCommit != commitX || started.EndCommit != "" || started.StartedAt == nil {
		t.Fatalf("the start commit's write answers start %q end %q started %v, want start %s, no end, and the run's start kept",
			started.StartCommit, started.EndCommit, started.StartedAt, commitX)
	}
	put(t, pool, endOf(implementer(id), ResultPassed, minute(20)), minute(20))
	ended := put(t, pool, commitsOf(implementer(id), "", commitY, ""), minute(20))
	if ended.StartCommit != commitX || ended.EndCommit != commitY || ended.Result != ResultPassed {
		t.Fatalf("the end commit's write answers start %q end %q result %q, want %s, %s and the run's result kept",
			ended.StartCommit, ended.EndCommit, ended.Result, commitX, commitY)
	}
	if commits := tuplesOf(t, pool, AttributeStartCommit, AttributeEndCommit); commits != 2 {
		t.Fatalf("the log holds %d tuples of the commits, want one for the start and one for the end", commits)
	}

	before := wholeLog(t, pool)
	again := put(t, pool, commitsOf(implementer(id), "", commitZ, ""), minute(30))
	if again.EndCommit != commitY {
		t.Fatalf("a second end at %s answers end %q, want the first, %s", commitZ, again.EndCommit, commitY)
	}
	if after := wholeLog(t, pool); !reflect.DeepEqual(after, before) {
		t.Fatalf("a second end appended %d tuples, want none", len(after)-len(before))
	}
	if read := stepOf(t, pool, "LEG-1", id); read.StartCommit != commitX || read.EndCommit != commitY || read.FixCheck != "" {
		t.Fatalf("the read lists start %q end %q fix %q, want %s to %s and no fix", read.StartCommit, read.EndCommit, read.FixCheck, commitX, commitY)
	}
}

// A fix names the check it fixes, once: the second run of the implementer's step takes the block
// id k-ci, a later write of another check leaves it, and the read lists it on that run alone; the
// first run, written without a check, names none. Commits written before the run's start make the
// record, as a rating does, and the start that lands after fills the rest.
func TestAFixRecordNamesItsCheck(t *testing.T) {
	pool := storetest.Open(t).Pool
	first, fix := uuid.NewString(), uuid.NewString()

	put(t, pool, endOf(startOf(implementer(first), minute(0)), ResultPassed, minute(10)), minute(10))
	named := put(t, pool, commitsOf(implementer(fix), commitX, "", "k-ci"), minute(30))
	if named.FixCheck != "k-ci" || named.StartCommit != commitX {
		t.Fatalf("the fix's write answers fix %q start %q, want k-ci at %s", named.FixCheck, named.StartCommit, commitX)
	}
	put(t, pool, startOf(implementer(fix), minute(30)), minute(30))
	before := wholeLog(t, pool)
	again := put(t, pool, commitsOf(implementer(fix), "", "", "k-other"), minute(31))
	if again.FixCheck != "k-ci" {
		t.Fatalf("a second check answers fix %q, want the first, k-ci", again.FixCheck)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("a second check appended %d tuples, want none", len(after)-len(before))
	}
	if nodes := countNodes(t, pool, fix); nodes != 1 {
		t.Fatalf("the fix's writes left %d nodes, want one", nodes)
	}

	steps := issueSteps(t, pool, "LEG-1")
	if len(steps) != 2 {
		t.Fatalf("the read lists %d runs, want the first run and the fix", len(steps))
	}
	for _, step := range steps {
		want := map[string]string{first: "", fix: "k-ci"}[step.ID]
		if step.FixCheck != want {
			t.Errorf("the read lists run %s (attempt %d) with fix %q, want %q", step.ID, step.Attempt, step.FixCheck, want)
		}
	}
	if fixed := stepOf(t, pool, "LEG-1", fix); fixed.Attempt != 2 || fixed.StartedAt == nil {
		t.Fatalf("the fix reads as attempt %d started %v, want the step's second run with its start", fixed.Attempt, fixed.StartedAt)
	}
}

// A commit is 40 lowercase hexadecimal digits, a fix_check has the form of a block id (any text
// but one holding U+0000), and only a run of kind step takes a fix_check, by the kind the write
// names and by the kind the record holds. Each refusal names the field and writes nothing. The
// same write with the values in their forms lands, which is the control: a block id holding a
// space and a colon is one.
func TestACommitsWriteRefusesAValueOutsideItsForm(t *testing.T) {
	pool := storetest.Open(t).Pool
	retro := uuid.NewString()
	put(t, pool, startOf(retroOf(retro), minute(0)), minute(0))
	before := wholeLog(t, pool)
	merge := retroOf(uuid.NewString())
	merge.Kind = new(StepKindMerge)
	// The retro's record, by its task, role and run alone: the write names no kind, so the kind the
	// record holds is the one judged.
	heldRetro := implementer(retro)
	heldRetro.Kind, heldRetro.Place, heldRetro.Type = nil, nil, nil
	for name, write := range map[string]StepWrite{
		"start_commit of 39 digits":             commitsOf(implementer(uuid.NewString()), commitX[:39], "", ""),
		"start_commit in upper case":            commitsOf(implementer(uuid.NewString()), strings.ToUpper(commitX), "", ""),
		"end_commit of a branch name":           commitsOf(implementer(uuid.NewString()), "", "main", ""),
		"end_commit of 41 digits":               commitsOf(implementer(uuid.NewString()), "", commitY+"a", ""),
		"fix_check holding U+0000":              commitsOf(implementer(uuid.NewString()), "", "", "k\x00ci"),
		"fix_check of a retro":                  commitsOf(retroOf(uuid.NewString()), "", "", "k-ci"),
		"fix_check of a merge":                  commitsOf(merge, "", "", "k-ci"),
		"fix_check of a record held as a retro": commitsOf(heldRetro, "", "", "k-ci"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tryPut(pool, write, minute(1))
			if !errors.Is(err, ErrStepInput) {
				t.Fatalf("the write answered %v, want %v", err, ErrStepInput)
			}
			if field := strings.Fields(name)[0]; !strings.Contains(err.Error(), field) {
				t.Errorf("the refusal %q does not name %s", err, field)
			}
		})
	}
	if after := wholeLog(t, pool); !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused writes moved the log from %d tuples to %d", len(before), len(after))
	}
	if held := stepOf(t, pool, "LEG-1", retro); held.FixCheck != "" {
		t.Fatalf("the retro holds fix %q, want none", held.FixCheck)
	}

	landed := put(t, pool, commitsOf(implementer(uuid.NewString()), commitX, commitY, "check 1: the unit job"), minute(2))
	if landed.StartCommit != commitX || landed.EndCommit != commitY || landed.FixCheck != "check 1: the unit job" {
		t.Fatalf("the write in its forms answers start %q end %q fix %q, want each as written", landed.StartCommit, landed.EndCommit, landed.FixCheck)
	}
}

// The read of a project counts each issue's fixes, the runs that name a fix_check, beside its
// failed steps: an issue whose implementer's step ran twice more as a fix of k-ci, after its tester
// failed once, reads two fixes and one failed step. The control is the same issue with the
// second fix written without its check, which reads one fix: a count of the step's runs, of its
// later attempts or of anything but the check would read two there too.
func TestTheProjectReadCountsFixes(t *testing.T) {
	for _, row := range []struct {
		name          string
		secondChecked bool
		fixes         int
	}{
		{"two runs naming a check", true, 2},
		{"the control: one of them names none", false, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := storetest.Open(t).Pool
			closed := t0.Add(2 * time.Hour)
			seedIssues(t, pool, "LEG", seededIssue{"LEG-1", "done", &closed})
			stepRun(t, pool, "LEG-1", StepKindStep, "implementer", 2, 1, new(minute(0)), new(minute(10)), ResultPassed)
			stepRun(t, pool, "LEG-1", StepKindStep, "tester", 3, 1, new(minute(10)), new(minute(20)), ResultFailed)
			for index, checked := range []bool{true, row.secondChecked} {
				id := stepRun(t, pool, "LEG-1", StepKindStep, "implementer", 2, 1,
					new(minute(30+10*index)), new(minute(35+10*index)), ResultPassed)
				check := ""
				if checked {
					check = "k-ci"
				}
				write := commitsOf(implementer(id), commitX, commitY, check)
				write.Type, write.Agent = nil, nil
				put(t, pool, write, minute(35+10*index))
			}
			read, err := ProjectPlans(context.Background(), pool, "LEG", Window{})
			if err != nil {
				t.Fatalf("read the plans of LEG: %v", err)
			}
			if len(read) != 1 || read[0].Issue != "LEG-1" {
				t.Fatalf("the read lists %v, want LEG-1 alone", read)
			}
			if read[0].Fixes != row.fixes || read[0].FailedSteps != 1 {
				t.Fatalf("LEG-1 reads fixes %d, failed steps %d, want fixes %d, failed steps 1", read[0].Fixes, read[0].FailedSteps, row.fixes)
			}
		})
	}
}
