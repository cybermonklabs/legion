package config

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A file that names `hooks:` runs the hooks it names and no default beside them, read whole and
// in file order: the first over every move back (`moves: any`) with its words in the key, the
// second over two named moves with the file they are read from. The file the second names is
// never written here, because this loader reads no file it names: the path is resolved against
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
