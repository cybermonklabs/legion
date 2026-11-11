package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// memoryFiles are a project's `memory.config_files` in every test here.
var memoryFiles = []string{"/etc/legion-memory/memory.yml", "/etc/legion-memory/recall.yml"}

// memoryConfig is a tmux configuration whose one project names the memory block at mode; an empty
// memory is a project that names no block at all.
func memoryConfig(memory config.Memory) config.Config {
	return config.Config{
		Project:  "test",
		Runtime:  config.Runtime{Name: "tmux"},
		Projects: map[string]config.Project{"test": {Memory: memory}},
	}
}

// memoryBlock is a project's whole `memory` block at mode.
func memoryBlock(mode config.MemoryMode) config.Memory {
	return config.Memory{Mode: mode, ConfigFiles: memoryFiles, InstructionsPath: "/etc/legion-memory/instructions.md"}
}

// memoryVariants are the two arms the `memory` block maps to, as each runtime's options carry
// them (config.Project.ExperimentsDeclared, declaredVariants): memory with the block's overlays,
// then none with no file at all.
var memoryVariants = []runtime.Variant{
	{Experiment: "memory", Name: "memory", ConfigFiles: memoryFiles},
	{Experiment: "memory", Name: "none"},
}

// fileless are the same two arms as a project that writes no `memory` block declares them: it
// declares the memory experiment all the same, at mode off, and neither arm names a file. They
// reach every runtime's options and name nothing there, since an arm with no overlay adds no
// PI_CONFIG_FILES value and no mount (runtime.RoleOverlayValues).
var fileless = []runtime.Variant{
	{Experiment: "memory", Name: "memory"},
	{Experiment: "memory", Name: "none"},
}

// A project that names overlays hands the arms that carry them to the tmux runtime whatever its
// mode is: the mode decides which new trees are drawn into the memory group, and a tree drawn
// into it before the mode moved to off still has files to name. A project with no block hands
// over the same two arms carrying no file, which names nothing in a pane: that is the pane every
// deployment built before this release.
func TestAProjectThatNamesOverlaysHandsThemToTheTmuxRuntimeAtEveryMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory config.Memory
		want   []runtime.Variant
	}{
		{"on", memoryBlock(config.MemoryOn), memoryVariants},
		{"random", memoryBlock(config.MemoryRandom), memoryVariants},
		{"off", memoryBlock(config.MemoryOff), memoryVariants},
		{"no mode", memoryBlock(""), memoryVariants},
		{"no block", config.Memory{}, fileless},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tmuxOptions(memoryConfig(tc.memory), "test", nil, "", "", quietLogger()).ExperimentVariants
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tmux ExperimentVariants = %v, want %v", got, tc.want)
			}
		})
	}
}

// The Sandbox runtime is handed the same list on the same rule, since a pod's role of a unit
// drawn into an arm names that arm's overlays whatever the project's mode is now.
func TestAProjectThatNamesOverlaysHandsThemToTheSandboxRuntimeAtEveryMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory config.Memory
		want   []runtime.Variant
	}{
		{"on", memoryBlock(config.MemoryOn), memoryVariants},
		{"random", memoryBlock(config.MemoryRandom), memoryVariants},
		{"off", memoryBlock(config.MemoryOff), memoryVariants},
		{"no mode", memoryBlock(""), memoryVariants},
		{"no block", config.Memory{}, fileless},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kubernetesConfig(t, "https://127.0.0.1:1")
			cfg.Projects = map[string]config.Project{cfg.Project: {Memory: tc.memory}}
			opts, err := sandboxOptions(cfg, *cfg.Runtime.Kubernetes, cfg.Project, "", lookup(nil), quietLogger())
			if err != nil {
				t.Fatalf("sandboxOptions: %v", err)
			}
			if !reflect.DeepEqual(opts.ExperimentVariants, tc.want) {
				t.Errorf("sandbox ExperimentVariants = %v, want %v", opts.ExperimentVariants, tc.want)
			}
		})
	}
}

// memorySpecs is a spec builder for a project whose memory instructions boot materialized, over
// a reader that gives the tree LEGION-1 the memory arm alone. The mode is no part of a launch's
// decision, so it is no part of this builder either.
func memorySpecs(t *testing.T, instructions string) specs {
	t.Helper()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	return specs{
		stateDir: t.TempDir(), project: "test", prompts: composer, repositories: noRepositoriesView,
		instructions: "/state/deployment-instructions.md",
		variants: func(_ context.Context, tree, _ string) ([]runtime.Variant, error) {
			if tree != "LEGION-1" {
				return nil, nil
			}
			return []runtime.Variant{{Experiment: "memory", Name: "memory", ConfigFiles: memoryFiles, InstructionsPath: instructions}}, nil
		},
	}
}

// memorySpawn is the spec s builds for role on issue of tree.
func memorySpawn(t *testing.T, s specs, tree, issue string, role claim.Role) runtime.SpawnSpec {
	t.Helper()
	token, err := claim.NewToken("test", issue, role)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Project: "test", Issue: issue, Tree: tree, Role: role})
	if err != nil {
		t.Fatalf("SpawnSpec %s: %v", token, err)
	}
	return spec
}

// inMemory is whether spec runs in the memory arm with instructions: the one variant the arm is,
// and that arm's instructions path among the prompt's.
func inMemory(spec runtime.SpawnSpec, instructions string) bool {
	want := []runtime.Variant{{Experiment: "memory", Name: "memory", ConfigFiles: memoryFiles, InstructionsPath: instructions}}
	var paths []string
	if instructions != "" {
		paths = []string{instructions}
	}
	return reflect.DeepEqual(spec.Variants, want) && reflect.DeepEqual(spec.Prompt.VariantInstructionsPaths, paths)
}

// Every role of a tree the draw put in the memory group launches in that arm: the spec says so,
// so each runtime names the arm's overlays last, and its prompt ends with the instructions the
// daemon materialized at boot, after the deployment's own. A role of a tree with no group, and
// the daemon's controller, carry neither.
func TestMemoryReachesOnlyTheRolesOfAMemoryTree(t *testing.T) {
	const instructions = "/state/experiments/memory/memory.md"
	s := memorySpecs(t, instructions)
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		spec := memorySpawn(t, s, "LEGION-1", "LEGION-2", role)
		if !inMemory(spec, instructions) {
			t.Errorf("the %s of a memory tree: variants = %+v, instructions = %q; want the memory arm and %q",
				role, spec.Variants, spec.Prompt.VariantInstructionsPaths, instructions)
		}
		if spec.Prompt.DeploymentInstructionsPath != s.instructions {
			t.Errorf("the %s of a memory tree: deployment instructions = %q, want %q", role, spec.Prompt.DeploymentInstructionsPath, s.instructions)
		}
	}
	noMemory := memorySpawn(t, s, "LEGION-7", "LEGION-8", claim.RoleTester)
	if len(noMemory.Variants) != 0 || len(noMemory.Prompt.VariantInstructionsPaths) != 0 {
		t.Errorf("a tree with no group: variants = %+v, instructions = %q; want neither", noMemory.Variants, noMemory.Prompt.VariantInstructionsPaths)
	}

	controller, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: claim.ControllerToken("test"), Project: "test", Role: claim.RoleController})
	if err != nil {
		t.Fatalf("the controller's SpawnSpec: %v", err)
	}
	if len(controller.Variants) != 0 || len(controller.Prompt.VariantInstructionsPaths) != 0 {
		t.Errorf("the controller: variants = %+v, instructions = %q; want neither", controller.Variants, controller.Prompt.VariantInstructionsPaths)
	}
}

// `mode: off` draws no new tree into the memory group and takes memory from no tree already
// drawn into it: the launch follows the record, so a tree the daemon recorded `memory` keeps its
// overlays and its instructions while the file still names that arm, whatever the mode is now.
// Only taking the arm out of the file takes the files away (dispatch://INFRA-990's spec: every
// tree keeps its group for its life; INFRA-1101's R3 and R6).
func TestAMemoryTreeKeepsItsFilesAfterTheProjectMovesToModeOff(t *testing.T) {
	const instructions = "/state/experiments/memory/memory.md"
	// The mode reaches no launch at all, so the one builder serves every mode; what the tmux and
	// Sandbox options hand over at off is the two tables above.
	off := memorySpecs(t, instructions)
	drawn := memorySpawn(t, off, "LEGION-1", "LEGION-1", claim.RoleTester)
	if !inMemory(drawn, instructions) {
		t.Errorf("a recorded memory tree at mode off: variants = %+v, instructions = %q; want the memory arm and %q",
			drawn.Variants, drawn.Prompt.VariantInstructionsPaths, instructions)
	}
	undrawn := memorySpawn(t, off, "LEGION-2", "LEGION-2", claim.RoleTester)
	if len(undrawn.Variants) != 0 || len(undrawn.Prompt.VariantInstructionsPaths) != 0 {
		t.Errorf("a tree with no draw at mode off: variants = %+v, instructions = %q; want neither",
			undrawn.Variants, undrawn.Prompt.VariantInstructionsPaths)
	}
}

// An arm with no instructions file is allowed: the tree's roles get the overlays and nothing is
// appended to their prompt. A reader that cannot answer refuses the launch rather than running
// the tree in no arm at all.
func TestMemoryWithoutInstructionsAppendsNothingAndAFailedReadRefuses(t *testing.T) {
	on := memorySpecs(t, "")
	spec := memorySpawn(t, on, "LEGION-1", "LEGION-2", claim.RoleTester)
	if !inMemory(spec, "") {
		t.Errorf("an arm with no instructions: variants = %+v, instructions = %q; want the memory arm and none",
			spec.Variants, spec.Prompt.VariantInstructionsPaths)
	}

	refused := memorySpecs(t, "/state/experiments/memory/memory.md")
	refused.variants = func(context.Context, string, string) ([]runtime.Variant, error) {
		return nil, errors.New("the store is down")
	}
	token, err := claim.NewToken("test", "LEGION-2", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	_, err = refused.SpawnSpec(context.Background(), supervise.Claim{Token: token, Project: "test", Issue: "LEGION-2", Tree: "LEGION-1", Role: claim.RoleTester})
	want := "the experiment variants of legion-test-legion-2-tester: the store is down"
	if err == nil || err.Error() != want {
		t.Fatalf("SpawnSpec over an unreadable record = %v, want %q", err, want)
	}
}

// The boot read of a project's memory overlays refuses a file no pane could read, naming the key
// and the file, as role_config_files' own read refuses one. It does not read the mode: a tree
// already drawn into the memory group names those overlays under any mode, so an unreadable one
// would stop its roles at off as surely as at on. A project with no overlays has none to read.
func TestMemoryOverlaysNoPaneCouldReadRefuseTheBoot(t *testing.T) {
	want := "projects.test.memory.config_files names /etc/legion-memory/absent.yml, which a role drawn into that arm could not read: "
	for _, mode := range []config.MemoryMode{config.MemoryOn, config.MemoryRandom, config.MemoryOff, ""} {
		cfg := memoryConfig(config.Memory{Mode: mode, ConfigFiles: []string{"/etc/legion-memory/absent.yml"}})
		err := checkRoleConfigFiles(cfg)
		if err == nil || len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
			t.Fatalf("checkRoleConfigFiles at mode %q = %v, want it to start %q", mode, err, want)
		}
	}

	none := memoryConfig(config.Memory{Mode: config.MemoryOff})
	if err := checkRoleConfigFiles(none); err != nil {
		t.Errorf("checkRoleConfigFiles over a block that names no overlay = %v, want nil", err)
	}
}

// The tmux boot gate reads each workflow role's whole PI_CONFIG_FILES, the project's memory
// overlays after that role's own, so a memory overlay Oh My Pi refuses beside a role's own file
// refuses the boot naming both keys, rather than failing that role's every launch. The controller
// is in no memory group, so its value is its own overlays alone and a memory overlay it would
// refuse stops nothing. The mode is not read: at off the gate probes the same value, since a tree
// already drawn into the memory group starts on it. A project with no overlays is probed as it
// was before the block existed.
func TestTheTmuxBootGateReadsAMemoryRolesWholeValue(t *testing.T) {
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	// The stub refuses exactly the pair: a value naming both the controller's file and a memory
	// overlay, which no pane is ever given, so only the gate's own composition can produce it.
	script := "#!/bin/sh\ncase \"$PI_CONFIG_FILES\" in *controller.yml*memory.yml*)\n" +
		"  echo \"error: Failed to parse config overlay $PI_CONFIG_FILES: SyntaxError\" >&2; exit 1;; esac\n" +
		"echo '{\"value\": false}'\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	controller, memory := filepath.Join(dir, "controller.yml"), filepath.Join(dir, "memory.yml")
	for _, file := range []string{controller, memory} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := capabilities.Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: dir}
	cfg := config.Config{
		Project:         "test",
		Runtime:         config.Runtime{Name: "tmux"},
		RoleConfigFiles: map[claim.Role][]string{claim.RoleController: {controller}},
		Projects: map[string]config.Project{"test": {
			Memory: config.Memory{Mode: config.MemoryOn, ConfigFiles: []string{memory}},
		}},
	}
	if err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger()); err != nil {
		t.Fatalf("checkPaneRoleOverlays = %v, want nil: the controller is in no memory group, so it is never given both", err)
	}

	// The same memory overlay given to a role that also names the file the stub refuses it beside.
	cfg.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {controller}}
	err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger())
	want := "boot gate: role_config_files.tester with projects.test.memory.config_files: the tester's Oh My Pi would not start: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("checkPaneRoleOverlays = %v, want it to start %q", err, want)
	}

	// At off the same value is probed, since a drawn tree's role still starts on it.
	cfg.Projects["test"] = config.Project{Memory: config.Memory{Mode: config.MemoryOff, ConfigFiles: []string{memory}}}
	if err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger()); err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("checkPaneRoleOverlays at mode off = %v, want it to start %q", err, want)
	}

	// A project that names no overlay is probed on the role's own files alone.
	cfg.Projects["test"] = config.Project{}
	if err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger()); err != nil {
		t.Fatalf("checkPaneRoleOverlays over a block that names no overlay = %v, want nil", err)
	}
}
