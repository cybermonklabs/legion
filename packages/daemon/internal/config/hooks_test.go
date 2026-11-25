package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A file that names `hooks:` runs the hooks it names and no default beside them, read whole and
// in file order: the first over every move back (`moves: any`) with its words in the key, the
// second over two named moves with the file they are read from. The file the second names is
// never written here, because this loader reads no hook's file: the path is resolved against
// the configuration's directory and kept (boot reads the words, ReadHookPrompt).
func TestLoadReadsTheHooks(t *testing.T) {
	path := writeConfigFile(t, minimalFile+`hooks:
  - name: stuck
    on: work_back
    moves: any
    every: 3
    roles: [implementer, tester]
    prompt: Say what the round missed.
  - name: back-to-implementing
    on: work_back
    moves: [reviewing-implementing, testing-implementing]
    every: 2
    roles: [implementer]
    prompt_file: hooks/back-to-implementing.md
`)

	cfg, err := Load(path, noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Hook{
		{
			Name: "stuck", On: HookEventWorkBack, Every: 3,
			Roles:  []claim.Role{claim.RoleImplementer, claim.RoleTester},
			Prompt: "Say what the round missed.",
		},
		{
			Name: "back-to-implementing", On: HookEventWorkBack, Every: 2,
			Moves:      []string{"reviewing-implementing", "testing-implementing"},
			Roles:      []claim.Role{claim.RoleImplementer},
			PromptFile: filepath.Join(filepath.Dir(path), "hooks/back-to-implementing.md"),
		},
	}
	if !reflect.DeepEqual(cfg.Hooks, want) {
		t.Errorf("Hooks = %+v, want %+v", cfg.Hooks, want)
	}
}

// A file that names no `hooks:` key runs the one hook the daemon ships (DefaultHooks), and one
// that writes the empty list runs none: the empty list is the operator switching the shipped hook
// off, which is why the key is read as a pointer. The default's words are pinned here, since they
// are what a deployment that configures nothing puts in front of its workers.
func TestLoadRunsTheShippedHookWithoutTheKey(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Hook{{
		Name: "work-back", On: "work_back", Every: 5,
		Roles:  []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer},
		Prompt: "We read no progress on this issue: its work has gone back the number of times recorded below. Report to the architect what is in your way, in your own words. The architect takes a step back, reads what each of you says, and decides the path. Carry on with the work while you wait, and say so if you read it as on track.",
	}}
	if !reflect.DeepEqual(cfg.Hooks, want) {
		t.Errorf("Hooks without the key = %+v, want the shipped hook %+v", cfg.Hooks, want)
	}

	off, err := Load(writeConfigFile(t, minimalFile+"hooks: []\n"), noEnv)
	if err != nil {
		t.Fatalf("Load an empty hooks list: %v", err)
	}
	if len(off.Hooks) != 0 {
		t.Errorf("Hooks on `hooks: []` = %+v, want no hook", off.Hooks)
	}
}

// aHook is minimalFile carrying the hooks the row writes, as a flow sequence so one row of
// TestLoadRefuses is one line: every refusal of a single hook names it `hooks[0]`.
func aHook(mappings string) string { return minimalFile + "hooks: [" + mappings + "]\n" }

// A task in another order than today's moves back by its own order, so a hook's `moves` names a
// move between any two different workflow phases: testing-reviewing, the move back of a task whose
// review comes before its test, loads. A step of a role the deployment defines runs as step, so a
// move into or out of step loads, and so does step-step, the move back from one such step to
// another; any other phase paired with itself is refused, since it is no move. The twin: a move
// back of today's line still loads.
func TestAHookNamesAMoveOfAnyOrder(t *testing.T) {
	for _, move := range []string{"testing-reviewing", "reviewing-implementing", "step-implementing", "implementing-step", "step-step"} {
		cfg, err := Load(writeConfigFile(t, aHook("{name: stuck, on: work_back, every: 1, moves: ["+move+"], roles: [tester], prompt: Words.}")), noEnv)
		if err != nil {
			t.Fatalf("Load(moves: [%s]): %v", move, err)
		}
		if len(cfg.Hooks) != 1 || !reflect.DeepEqual(cfg.Hooks[0].Moves, []string{move}) {
			t.Fatalf("Hooks of moves: [%s] = %+v, want the one move", move, cfg.Hooks)
		}
	}
	_, err := Load(writeConfigFile(t, aHook("{name: stuck, on: work_back, every: 1, moves: [testing-testing], roles: [tester], prompt: Words.}")), noEnv)
	want := `hooks[0].moves names "testing-testing", which is no move the daemon counts: a move is written "<from>-<to>", two workflow phases, which differ but in step-step`
	if err == nil || err.Error() != want {
		t.Fatalf("Load(moves: [testing-testing]) error = %v, want %q", err, want)
	}
}

// A hook that names a move no task's line makes back would never fire, so the loader refuses it,
// naming the move: each step phase, step among them, to retro or to merging, and retro to merging,
// since the retro and the merge end every line. These 13 are written out here, not read from the
// code under test. The twin: each of the other 44 pairs of MovePairs loads, a move back of today's
// line, one of another order (implementing-testing, where the tester stands before the
// implementer, among them) and one of a step of a role the deployment defines alike.
func TestAHookNamingAMoveNoLineMakesBackIsRefused(t *testing.T) {
	never := []string{
		"planning-retro", "implementing-retro", "testing-retro", "reviewing-retro", "accepting-retro",
		"planning-merging", "implementing-merging", "testing-merging", "reviewing-merging", "accepting-merging",
		"retro-merging", "step-retro", "step-merging",
	}
	loaded := 0
	for _, move := range phase.MovePairs() {
		_, err := Load(writeConfigFile(t, aHook("{name: stuck, on: work_back, every: 1, moves: ["+move+"], roles: [tester], prompt: Words.}")), noEnv)
		if !slices.Contains(never, move) {
			if err != nil {
				t.Errorf("Load(moves: [%s]): %v, want the move back loaded", move, err)
			}
			loaded++
			continue
		}
		want := fmt.Sprintf(`hooks[0].moves names %q, which no task's line makes as a move back, so the hook would never fire: retro and merging end every line`, move)
		if err == nil || err.Error() != want {
			t.Errorf("Load(moves: [%s]) error = %v, want %q", move, err, want)
		}
	}
	if loaded != 44 {
		t.Errorf("%d pairs were read as moves back, want 44", loaded)
	}
}
