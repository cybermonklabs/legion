package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
// off, which is why the key is read as a pointer. The shipped hook's words are its constant,
// DefaultWorkBackPrompt: what they say is prose for the roles, which no unit test holds.
func TestLoadRunsTheShippedHookWithoutTheKey(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Hook{{
		Name: "work-back", On: "work_back", Every: 5,
		Roles:  []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer},
		Prompt: DefaultWorkBackPrompt,
	}}
	if !reflect.DeepEqual(cfg.Hooks, want) {
		t.Errorf("Hooks without the key = %+v, want the shipped hook %+v", cfg.Hooks, want)
	}
	if DefaultWorkBackPrompt == "" {
		t.Error("the shipped hook carries no words, so it would put nothing in front of its roles")
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
// another; any other phase paired with itself is refused, since it is no move, by its place in the
// list, as every word that is no move is. The twin: a move back of today's line still loads.
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
	want := `hooks[0].moves entry 1 is no move the daemon counts: a move is written "<from>-<to>", two workflow phases, which differ but in step-step`
	if err == nil || err.Error() != want {
		t.Fatalf("Load(moves: [testing-testing]) error = %v, want %q", err, want)
	}
}

// A hook that names a move no task's line makes back would never fire, so the loader refuses it,
// naming the move: each step phase, step among them, to retro or to merging, and retro to merging,
// since the retro and the merge end every line; and implementing to testing, reviewing or
// accepting, since every tester, reviewer and acceptance step comes after the implementer's. These
// 16 are written out here, not read from the code under test. The twin: each of the other 41
// pairs of MovePairs loads, a move back of today's line, one of another order and one of a step of
// a role the deployment defines alike.
func TestAHookNamingAMoveNoLineMakesBackIsRefused(t *testing.T) {
	never := []string{
		"planning-retro", "implementing-retro", "testing-retro", "reviewing-retro", "accepting-retro",
		"planning-merging", "implementing-merging", "testing-merging", "reviewing-merging", "accepting-merging",
		"retro-merging", "implementing-testing", "implementing-reviewing", "implementing-accepting",
		"step-retro", "step-merging",
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
		want := fmt.Sprintf(`hooks[0].moves names %q, which no task's line makes as a move back, so the hook would never fire: retro and merging end every line, and every tester, reviewer and acceptance step comes after the implementer's`, move)
		if err == nil || err.Error() != want {
			t.Errorf("Load(moves: [%s]) error = %v, want %q", move, err, want)
		}
	}
	if loaded != 41 {
		t.Errorf("%d pairs were read as moves back, want 41", loaded)
	}
}

// Boot reads a hook's prompt_file (ReadHookPrompt) and refuses one that is missing or blank,
// naming the hook by its name where the name has a name's shape (nameShape: `stuck`,
// `back-to-implementing`) and by its place otherwise (`Stuck`), and naming the path, and the
// read's error, which repeats it, where the file writes prompt_file in a path's shape
// (`prompts/missing.md`), and the key and why alone otherwise (`Prompts/Missing.md`).
func TestReadHookPromptNamesTheHookAndThePathWhereEachHasItsShape(t *testing.T) {
	for _, tc := range []struct {
		name, hook, promptFile string
		blank                  bool
		want                   func(path string) string
	}{
		{name: "a name and a path of their shapes", hook: "stuck", promptFile: "prompts/missing.md",
			want: func(path string) string {
				return "hook stuck prompt_file " + path + " could not be read: open " + path + ": no such file or directory"
			}},
		{name: "a name of the role grammar", hook: "back-to-implementing", promptFile: "prompts/missing.md",
			want: func(path string) string {
				return "hook back-to-implementing prompt_file " + path + " could not be read: open " + path + ": no such file or directory"
			}},
		{name: "a name of no name's shape", hook: "Stuck", promptFile: "prompts/missing.md",
			want: func(path string) string {
				return "hooks[0].prompt_file " + path + " could not be read: open " + path + ": no such file or directory"
			}},
		{name: "a path of no path's shape", hook: "stuck", promptFile: "Prompts/Missing.md",
			want: func(string) string {
				return "hook stuck prompt_file names a file that could not be read: it does not exist"
			}},
		{name: "a blank file named in a path's shape", hook: "stuck", promptFile: "prompts/blank.md", blank: true,
			want: func(path string) string { return "hook stuck prompt_file " + path + " is empty" }},
		{name: "a blank file named in no path's shape", hook: "stuck", promptFile: "Prompts/Blank.md", blank: true,
			want: func(string) string { return "hook stuck prompt_file names a file that is empty" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := writeConfigFile(t, aHook("{name: "+tc.hook+", on: work_back, every: 1, roles: [implementer], prompt_file: "+tc.promptFile+"}"))
			path := filepath.Join(filepath.Dir(config), tc.promptFile)
			if tc.blank {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(config, noEnv)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			_, err = ReadHookPrompt(0, cfg.Hooks[0])
			if want := tc.want(path); err == nil || err.Error() != want {
				t.Fatalf("ReadHookPrompt = %v, want %q", err, want)
			}
			if strings.Contains(tc.promptFile, "P") && strings.Contains(err.Error(), filepath.Base(path)) {
				t.Fatalf("ReadHookPrompt = %v names the path written in no path's shape", err)
			}
		})
	}
}
