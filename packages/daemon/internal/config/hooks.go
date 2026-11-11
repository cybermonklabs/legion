package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// hooksKey is `hooks`, the key every refusal of it names, each with the position of the hook in
// the list: `hooks[0].every`.
const hooksKey = "hooks"

// HookEventWorkBack is the one event that ships: the work of an issue went back a phase, which
// the daemon counts by its `<from>-<to>` pair (phase.BackwardPairs).
const HookEventWorkBack = "work_back"

// HookEvents are the events a hook's `on` may name: the daemon's own set of events, which the
// loader refuses anything outside of. The documentation lists the same set, so the two are read
// together. A new event here is a change to Legion, which the daemon must raise and the
// documentation must name; a new hook is a deployment's own edit of its `legion.yaml`, which
// needs no change to this list.
var HookEvents = []string{HookEventWorkBack}

// Hook is one entry of `hooks`, the deployment's own prompt in front of named roles every Every
// moves back. PromptFile is kept as a path: this loader reads no file it names, and boot reads
// the words (ReadHookPrompt, daemon.readBoot).
type Hook struct {
	Name       string
	On         string   // the event; work_back is the one that ships
	Moves      []string // empty selects every backward move (`moves: any`)
	Every      int
	Roles      []claim.Role
	Prompt     string // the words, inline
	PromptFile string // or the path boot reads them from, resolved against the config directory
}

// DefaultWorkBackPrompt is the words of the one hook a deployment that writes no `hooks:` key
// runs (DefaultHooks). It is prose for the role that reads it, and it asks for one thing: that
// the worker says what is in its way, so the architect can read all three answers together.
const DefaultWorkBackPrompt = "We read no progress on this issue: its work has gone back the number of times recorded below. Report to the architect what is in your way, in your own words. The architect takes a step back, reads what each of you says, and decides the path. Carry on with the work while you wait, and say so if you read it as on track."

// DefaultHooks is the hook a deployment that names no `hooks:` key runs: every fifth move back of
// any kind puts DefaultWorkBackPrompt in front of the three roles that do the work of a round.
// The architect is not among them, and no hook may name it: the daemon's own wake to an architect
// is a notice. The answer is a fresh slice, so a caller that sorts or trims it leaves the next
// caller's answer whole.
func DefaultHooks() []Hook {
	return []Hook{{
		Name:   "work-back",
		On:     HookEventWorkBack,
		Every:  5,
		Roles:  []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer},
		Prompt: DefaultWorkBackPrompt,
	}}
}

// readHooks is `hooks`: a list of mappings, each one hook of the deployment. It answers a pointer
// so that the settle can tell a file that names no key, which runs DefaultHooks(), from one that
// writes `hooks: []`, which runs none. A null key (`hooks:` and nothing under it) is the key
// unset, as every other key of this loader reads a null, so the default stands.
func readHooks(value *yaml.Node, key string) (*[]Hook, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	hooks := make([]Hook, 0, len(value.Content))
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		hook, err := readHook(entry, field)
		if err != nil {
			return nil, err
		}
		for earlier, named := range hooks {
			if named.Name == hook.Name {
				return nil, fmt.Errorf("%s.name is %q, which %s[%d] already carries: a hook is named once, and its name is what every prompt it publishes carries",
					field, hook.Name, key, earlier)
			}
		}
		hooks = append(hooks, hook)
	}
	return &hooks, nil
}

// readHook is one entry of the list, refused naming its position and the member at fault. The
// words are the hook's own (`prompt`) or the file they are read from (`prompt_file`), never both
// and never neither: a hook with no words would fire and say nothing.
func readHook(entry *yaml.Node, field string) (Hook, error) {
	if entry.Kind != yaml.MappingNode {
		return Hook{}, fmt.Errorf("%s must be a mapping", field)
	}
	fields, err := describedMembers(entry, field, hooksKey+"[]")
	if err != nil {
		return Hook{}, err
	}
	var hook Hook
	if hook.Name, err = requiredString(fields["name"], field+".name", ": the hook's own name, which every prompt it publishes carries"); err != nil {
		return Hook{}, err
	}
	if hook.On, err = requiredString(fields["on"], field+".on", ": the event the hook stands on"); err != nil {
		return Hook{}, err
	}
	if !slices.Contains(HookEvents, hook.On) {
		return Hook{}, fmt.Errorf("%s.on is %q, which is no event the daemon has (%s)", field, hook.On, strings.Join(HookEvents, ", "))
	}
	if hook.Every, err = readHookEvery(fields["every"], field); err != nil {
		return Hook{}, err
	}
	if hook.Roles, err = readHookRoles(fields["roles"], field); err != nil {
		return Hook{}, err
	}
	if hook.Moves, err = readHookMoves(fields["moves"], field); err != nil {
		return Hook{}, err
	}
	if hook.Prompt, err = optionalString(fields["prompt"], field+".prompt"); err != nil {
		return Hook{}, err
	}
	if hook.PromptFile, err = optionalString(fields["prompt_file"], field+".prompt_file"); err != nil {
		return Hook{}, err
	}
	switch {
	case hook.Prompt != "" && hook.PromptFile != "":
		return Hook{}, fmt.Errorf("%s names prompt and prompt_file: the hook's words are written in the key, or read from a file, never both", field)
	case hook.Prompt == "" && hook.PromptFile == "":
		return Hook{}, fmt.Errorf("%s names neither prompt nor prompt_file: a hook carries the words it puts in front of its roles", field)
	}
	return hook, nil
}

// readHookEvery is a hook's `every`: how many moves back of what the hook selects fire it. It is
// required, because a hook that fired on no count would fire on none or on every one, and neither
// is what the operator wrote the key for.
func readHookEvery(value *yaml.Node, field string) (int, error) {
	every, err := readInt(value, field+".every")
	if err != nil || (every != nil && *every < 1) {
		return 0, fmt.Errorf("%s.every must be a positive whole number: the hook fires on every nth move back it selects", field)
	}
	if every == nil {
		return 0, fmt.Errorf("%s.every is required: the hook fires on every nth move back it selects", field)
	}
	return *every, nil
}

// readHookRoles is a hook's `roles`: the roles its words are put in front of, at least one, each
// a Legion role of an issue. The architect is refused: the daemon's own wake to an architect is a
// notice it writes itself, and a hook writes none.
func readHookRoles(value *yaml.Node, field string) ([]claim.Role, error) {
	key := field + ".roles"
	if value == nil {
		return nil, errors.New(key + " is required: the hook puts its words in front of the roles it names")
	}
	named, err := readStrings(value, key)
	if err != nil {
		return nil, err
	}
	if len(named) == 0 {
		return nil, errors.New(key + " names no role: the hook puts its words in front of the roles it names")
	}
	roles := make([]claim.Role, 0, len(named))
	for _, name := range named {
		role := claim.Role(name)
		if !slices.Contains(claim.Roles, role) {
			return nil, fmt.Errorf("%s names %q, which is not a Legion role (%s)", key, name, runtime.JoinRoles(claim.Roles))
		}
		if role == claim.RoleArchitect {
			return nil, fmt.Errorf("%s names the architect, which no hook writes to: the daemon's own wake to an architect is a notice", key)
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// readHookMoves is a hook's `moves`: the moves back it selects, each written "<from>-<to>" as the
// daemon counts them (phase.BackwardPairs). The key left out, and the word `any`, each select
// every move back, which is the empty list this answers. The empty list in the file is refused
// instead: it selects nothing, so the hook could never fire, and the operator meant `any` or
// meant to name a move.
func readHookMoves(value *yaml.Node, field string) ([]string, error) {
	key := field + ".moves"
	if value == nil {
		return nil, nil
	}
	if value.Kind == yaml.ScalarNode {
		if value.Value == "any" {
			return nil, nil
		}
		return nil, fmt.Errorf("%s must be an array of the moves back it selects, or the word any", key)
	}
	moves, err := readStrings(value, key)
	if err != nil {
		return nil, fmt.Errorf("%s must be an array of the moves back it selects, or the word any", key)
	}
	if len(moves) == 0 {
		return nil, fmt.Errorf("%s names no move back: write any, or leave the key out, to select every move back", key)
	}
	for _, move := range moves {
		if _, _, ok := phase.ParsePair(move); !ok {
			return nil, fmt.Errorf("%s names %q, which is no move back the daemon counts: a move is written \"<from>-<to>\", both of them workflow phases, and the phase it goes to before the phase it leaves", key, move)
		}
	}
	return moves, nil
}

// settleHooks is `hooks` settled: the list the file names, no hook where it writes the empty
// list, and DefaultHooks() where it names no key at all. Each `prompt_file` is resolved against
// the directory of the file that set it, as `instructions` is (underConfig), and is kept as a
// path: boot reads the words.
func settleHooks(named *[]Hook, configDir string) []Hook {
	if named == nil {
		return DefaultHooks()
	}
	hooks := *named
	for index := range hooks {
		if hooks[index].PromptFile != "" {
			hooks[index].PromptFile = underConfig(hooks[index].PromptFile, configDir)
		}
	}
	return hooks
}
