package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's lines of a launch of an arm's folder of texts (textsReports): one that read the
// folder, one that ran its fallback, and the outcome of the notice that fallback applies.
const (
	textsReadLine           = "legion daemon launches with an arm's folder of texts"
	textsFallbackLine       = "legion daemon launched Legion's own texts in place of an arm's folder of texts"
	textsNoticedLine        = "legion daemon applied the notice of a launch that ran Legion's own texts in place of an arm's folder of texts"
	textsNoticedAgainLine   = "legion daemon applied no second notice of a launch that ran Legion's own texts in place of an arm's folder of texts"
	textsNoticeFailedLine   = "apply the notice of a launch that ran Legion's own texts in place of an arm's folder of texts"
	textsProject            = "legion"
	textsLongerInstructions = "/etc/legion/longer.md"
)

// textsExperiments are the two experiments every launch below may run in, in the file's order:
// `longer`, whose arm `on` names overlays and instructions and no folder, and `shorter`, reaching
// roles (nil for every issue role), whose arm `folder` names overlays, instructions and the folder
// of texts folder, beside a control that names nothing. A fallback drops `shorter`'s arm whole and
// keeps `longer`'s.
func textsExperiments(roles []claim.Role, folder string) []config.Experiment {
	return []config.Experiment{
		{Name: "longer", Mode: config.ExperimentRandom, Unit: config.ExperimentTree, Variants: []config.Variant{
			{Name: "on", Weight: 1, ConfigFiles: []string{"/etc/legion/longer.yml"}, InstructionsPath: textsLongerInstructions,
				ConfigFilesField: "projects.LEGION.experiments.longer.variants.on.config_files"},
			{Name: "off", Weight: 1},
		}},
		{Name: "shorter", Mode: config.ExperimentRandom, Unit: config.ExperimentTree, Roles: roles, Variants: []config.Variant{
			{Name: "folder", Weight: 1, ConfigFiles: []string{"/etc/legion/shorter.yml"}, InstructionsPath: "/etc/legion/shorter.md", TextsPath: folder,
				ConfigFilesField: "projects.LEGION.experiments.shorter.variants.folder.config_files"},
			{Name: "control", Weight: 1},
		}},
	}
}

// textsRig is the daemon's spec builder over the shipped prompts and a state directory of its own,
// launching the roles of tree, whose units the record puts in `longer`'s arm `on` and in whichever
// arm of `shorter` a launch names (textsExperiments). It checks names against what the worker image
// ships (shipped-names.txt) and logs into logs as JSON; a rig told through a pool (tellThrough)
// tells the issue of each fallback as a daemon with a workflow does.
type textsRig struct {
	t      *testing.T
	specs  specs
	logs   *syncBuffer
	launch experiment.Launch
	tree   string
}

func newTextsRig(t *testing.T, tree string, roles []claim.Role, folder string) *textsRig {
	t.Helper()
	return newTextsRigOf(t, tree, textsExperiments(roles, folder))
}

func newTextsRigOf(t *testing.T, tree string, declared []config.Experiment) *textsRig {
	t.Helper()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	stateDir := t.TempDir()
	// The daemon's copy of each arm's instructions, as boot writes it (writeInstructionsCopies),
	// which a launch's prompt ends with.
	for _, e := range declared {
		for _, variant := range e.Variants {
			if variant.InstructionsPath == "" {
				continue
			}
			copied := config.VariantInstructionsFile(stateDir, e.Name, variant)
			if err := os.MkdirAll(filepath.Dir(copied), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(copied, []byte(textsInstructions(e.Name, variant.Name)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	logs := &syncBuffer{}
	shipped := texts.NewShipped(shippedNamesFixture(t))
	return &textsRig{t: t, logs: logs, tree: tree, launch: experiment.ForLaunch(declared, stateDir),
		specs: specs{stateDir: stateDir, project: textsProject, prompts: composer, appOwners: noAppOwners,
			shipped: func() texts.Shipped { return shipped },
			texts:   &textsReports{log: slog.New(slog.NewJSONHandler(logs, nil))}}}
}

// textsInstructions is the text of the instructions of experiment's arm a rig's launch ends with.
func textsInstructions(experiment, arm string) string {
	return fmt.Sprintf("The instructions of %s's arm %s.\n", experiment, arm)
}

// tellThrough binds the rig's reports to a workflow over pool, as the boot binds a daemon's once
// its workflow is open (textsReports.bind).
func (r *textsRig) tellThrough(pool *store.Pool) {
	r.specs.texts.bind(pool, textsHandlers(r.t, pool))
}

// spawn is the spec the rig builds for role's launch on issue, an issue of the rig's tree, at
// generation, in a unit the record puts in `longer`'s arm `on` and in `shorter`'s arm drawn, "" for
// none of `shorter`'s.
func (r *textsRig) spawn(drawn, issue string, role claim.Role, generation uint64) runtime.SpawnSpec {
	r.t.Helper()
	recorded := map[string][]record.Assignment{r.tree: {{Experiment: "longer", Variant: "on"}}}
	if drawn != "" {
		recorded[r.tree] = append(recorded[r.tree], record.Assignment{Experiment: "shorter", Variant: drawn})
	}
	s := r.specs
	s.variants = func(_ context.Context, tree, issue string, role claim.Role) (experiment.Arms, error) {
		return r.launch.Arms(recorded, tree, issue, role), nil
	}
	return r.spawnWith(s, issue, role, generation)
}

// spawnWith is the spec s builds for role's launch on issue at generation.
func (r *textsRig) spawnWith(s specs, issue string, role claim.Role, generation uint64) runtime.SpawnSpec {
	r.t.Helper()
	spec, err := s.SpawnSpec(context.Background(), r.claim(issue, role, generation))
	if err != nil {
		r.t.Fatalf("the spec of the %s on %s: %v", role, issue, err)
	}
	return spec
}

// claim is role's claim on issue, an issue of the rig's tree, at generation.
func (r *textsRig) claim(issue string, role claim.Role, generation uint64) supervise.Claim {
	return supervise.Claim{Token: mustClaimToken(r.t, issue, role), Project: textsProject, Tree: r.tree, Issue: issue, Role: role, Generation: generation}
}

// legionParts are the snapshots of Legion's own parts of role, as a launch in no folder reads them.
func (r *textsRig) legionParts(role claim.Role, isRoot bool) []string {
	r.t.Helper()
	parts, err := r.specs.prompts.Compose(role, isRoot)
	if err != nil {
		r.t.Fatal(err)
	}
	return parts.RolePromptPaths
}

// lines are the rig's log lines whose message starts with msg, of token's launches.
func (r *textsRig) lines(msg string, token claim.Token) []map[string]any {
	r.t.Helper()
	return slices.DeleteFunc(loggedMessages(r.t, r.logs.String(), msg), func(line map[string]any) bool {
		return line["claim"] != string(token)
	})
}

// awaitNotices waits until the rig's reports logged the outcome of n notices of token's launches,
// applied or applied already, and fails at once on one that could not be applied.
func (r *textsRig) awaitNotices(token claim.Token, n int) {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("%d notices of %s's fallbacks to be applied", n, token), func() bool {
		if failed := r.lines(textsNoticeFailedLine, token); len(failed) > 0 {
			r.t.Fatalf("a notice of %s's fallback could not be applied: %v", token, failed)
		}
		return len(r.lines(textsNoticedLine, token))+len(r.lines(textsNoticedAgainLine, token)) >= n
	})
}

// composed is the system prompt a runtime composes from spec's parts (runtime.ComposePrompt).
func composed(t *testing.T, spec runtime.SpawnSpec) string {
	t.Helper()
	text, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		t.Fatalf("compose the prompt: %v", err)
	}
	return text
}

// textsHandlers are the handlers a daemon's workflow applies a fact through, over pool's records.
func textsHandlers(t *testing.T, pool *store.Pool) []intake.Handler {
	t.Helper()
	return []intake.Handler{workflow.New(record.NewStore(), workflow.Config{Project: textsProject, Repositories: testRepositories}, quietLogger())}
}

// seedTextsIssue records key, a tree's root, as the workflow's issue.
func seedTextsIssue(t *testing.T, pool *store.Pool, key string) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: key, Title: key, Phase: phase.Reviewing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U"})
	}); err != nil {
		t.Fatalf("record %s: %v", key, err)
	}
}

// textsMessages are the bodies of the messages the outbox holds for issue, oldest first, each
// decoded as the outbox decodes its row.
func textsMessages(t *testing.T, pool *store.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select id, kind, issue, payload from outbox where kind = $1 and issue = $2 order by id",
		string(record.OutboxKindDispatchMessage), issue)
	if err != nil {
		t.Fatalf("read the messages of %s: %v", issue, err)
	}
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("read a message of %s: %v", issue, err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode message %d: %v", row.ID, err)
		}
		bodies = append(bodies, payload.(record.MessagePost).Body)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the messages of %s: %v", issue, err)
	}
	return bodies
}

// textsFacts is how many fallback facts of token's launches intake marked applied.
func textsFacts(t *testing.T, pool *store.Pool, token claim.Token) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "select count(*) from processed_events where source = 'supervise' and event_id like $1",
		"texts-fallback/"+string(token)+"/%").Scan(&n); err != nil {
		t.Fatalf("count the fallback facts of %s: %v", token, err)
	}
	return n
}

// reviewerFolder is a folder that replaces Legion's roles/reviewer.md with text, its manifest
// recording Legion's digest at that path.
func reviewerFolder(t *testing.T, text string) string {
	t.Helper()
	return textsFolder(t, map[string]string{"roles/reviewer.md": text}, map[string]string{"roles/reviewer.md": legionDigest(t, "roles/reviewer.md")})
}

// fingerprintOf is the fingerprint texts.Read reads folder at, "" for a folder it refuses.
func fingerprintOf(t *testing.T, folder string) string {
	t.Helper()
	read, err := texts.Read(folder)
	if err != nil {
		return ""
	}
	return read.Fingerprint
}

// wantFallbackLine holds the rig's log to one line of token's launch at generation falling back
// from folder, naming the experiment and the arm, the file ("none" for the folder as a whole), the
// cause and the fingerprint it read the folder at.
func (r *textsRig) wantFallbackLine(token claim.Token, generation uint64, folder, file string, cause texts.Cause, fingerprint string) {
	r.t.Helper()
	lines := r.lines(textsFallbackLine, token)
	if len(lines) != 1 {
		r.t.Fatalf("the daemon logged %d fallbacks of %s, want one: %v", len(lines), token, lines)
	}
	if file == "" {
		file = "none"
	}
	want := map[string]any{"level": "WARN", "generation": float64(generation), "experiment": "shorter", "arm": "folder", "folder": folder,
		"file": file, "cause": string(cause), "fingerprint": fingerprint}
	for key, value := range want {
		if lines[0][key] != value {
			r.t.Errorf("the fallback line's %s = %v, want %v (line %v)", key, lines[0][key], value, lines[0])
		}
	}
}

// Each launch of a role the experiment reaches reads the folder's text of each of its parts the
// folder holds, from the daemon's copy, and Legion's own text of every other, each part once and
// in Legion's order: for every role, in either of its shapes (a root architect and a child one
// read different parts). The folder holds every other part of the role's, at Legion's digests, so
// both kinds are read in each launch. The spec carries the folder, its fingerprint and the copy,
// and is one a runtime takes (runtime.ValidateSpawnSpec) once the machine names its claim.
func TestEveryReachedRolesLaunchReadsTheFolderInPlaceOfLegions(t *testing.T) {
	t.Parallel()
	for _, role := range claim.Roles {
		for _, isRoot := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s root=%v", role, isRoot), func(t *testing.T) {
				t.Parallel()
				names, err := prompts.PartNames(role, isRoot)
				if err != nil {
					t.Fatal(err)
				}
				files, manifest := map[string]string{}, map[string]string{}
				for i, name := range names {
					if i%2 == 0 {
						files[name] = fmt.Sprintf("The folder's text of %s for the %s.\n", name, role)
						manifest[name] = legionDigest(t, name)
					}
				}
				folder := textsFolder(t, files, manifest)
				r := newTextsRig(t, "LEGION-1", nil, folder)
				issue := "LEGION-2"
				if isRoot {
					issue = r.tree
				}
				spec := r.spawn("folder", issue, role, 3)
				if spec.Texts == nil {
					t.Fatalf("the launch carries no folder, want %s: logged %s", folder, r.logs.String())
				}
				token := mustClaimToken(t, issue, role)
				copied := filepath.Join(r.specs.stateDir, "texts", string(token), "3", "folder")
				if spec.Texts.Folder != folder || spec.Texts.Copy != copied || spec.Texts.Fingerprint != fingerprintOf(t, folder) ||
					spec.Texts.Experiment != "shorter" || spec.Texts.Variant != "folder" {
					t.Errorf("the launch carries %+v, want the folder %s, its fingerprint and the copy %s", spec.Texts, folder, copied)
				}

				legion := r.legionParts(role, isRoot)
				want := slices.Clone(legion)
				for i, name := range names {
					if _, held := files[name]; held {
						want[i] = filepath.Join(copied, filepath.FromSlash(name))
					}
				}
				if !reflect.DeepEqual(spec.Prompt.RolePromptPaths, want) {
					t.Fatalf("the launch reads %q, want %q", spec.Prompt.RolePromptPaths, want)
				}
				prompt := composed(t, spec)
				for i, name := range names {
					legionText, err := fs.ReadFile(prompts.Texts(), name)
					if err != nil {
						t.Fatal(err)
					}
					own := strings.TrimSpace(string(legionText))
					if folderText, held := files[name]; held {
						if got := strings.Count(prompt, strings.TrimSpace(folderText)); got != 1 {
							t.Errorf("the prompt holds the folder's %s %d times, want once", name, got)
						}
						if own != "" && strings.Contains(prompt, own) {
							t.Errorf("the prompt holds Legion's own %s beside the folder's", name)
						}
					} else if own != "" && !strings.Contains(prompt, own) {
						t.Errorf("the prompt lacks Legion's own %s (part %d), which the folder does not hold", name, i)
					}
				}

				launched := spec
				launched.Claim, launched.Project, launched.Tree, launched.Issue, launched.Role = token, textsProject, r.tree, issue, role
				launched.Generation, launched.BootToken = 3, "boot"
				if err := runtime.ValidateSpawnSpec(launched, nil); err != nil {
					t.Errorf("a runtime refuses the launch: %v", err)
				}
			})
		}
	}
}

// A role the experiment's `roles` leaves out runs in none of its arms, whatever its tree drew: its
// launch reads Legion's own parts, names no arm of the experiment and carries no folder, and the
// daemon copies nothing for it, while the role the experiment names reads the folder.
func TestALaunchOfARoleTheExperimentLeavesOutReadsLegionsTexts(t *testing.T) {
	t.Parallel()
	r := newTextsRig(t, "LEGION-1", []claim.Role{claim.RoleReviewer}, reviewerFolder(t, "The folder's reviewer.\n"))
	for _, role := range claim.Roles {
		if role == claim.RoleReviewer {
			continue
		}
		spec := r.spawn("folder", "LEGION-2", role, 2)
		if spec.Texts != nil || !reflect.DeepEqual(spec.Prompt.RolePromptPaths, r.legionParts(role, false)) {
			t.Errorf("the %s, whom the experiment leaves out, reads %q with the folder %+v; want Legion's own parts and no folder",
				role, spec.Prompt.RolePromptPaths, spec.Texts)
		}
		if slices.ContainsFunc(spec.Variants, func(v runtime.Variant) bool { return v.Experiment == "shorter" }) {
			t.Errorf("the %s runs in the arms %+v, want none of the experiment that leaves it out", role, spec.Variants)
		}
		if _, err := os.Stat(textsCopies(r.specs.stateDir, mustClaimToken(t, "LEGION-2", role))); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the daemon keeps a copy for the %s (%v), whose launch reads no folder", role, err)
		}
	}
	if spec := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 2); spec.Texts == nil {
		t.Errorf("the reviewer, whom the experiment names, carries no folder; logged %s", r.logs.String())
	}
}

// A unit the draw put in the experiment's control arm, which names no file and no folder, is
// launched as one in no arm of the experiment: Legion's own parts, the same prompt, no folder and no
// copy, its one arm of that experiment naming nothing a runtime adds.
func TestALaunchOfTheControlArmIsTheLaunchOfNoExperiment(t *testing.T) {
	t.Parallel()
	r := newTextsRig(t, "LEGION-1", nil, reviewerFolder(t, "The folder's reviewer.\n"))
	control := r.spawn("control", "LEGION-2", claim.RoleReviewer, 2)
	none := r.spawn("", "LEGION-2", claim.RoleReviewer, 2)
	if control.Texts != nil || !reflect.DeepEqual(control.Prompt, none.Prompt) {
		t.Fatalf("the control arm's launch reads %+v with the folder %+v; want the prompt of a launch in no arm, %+v, and no folder",
			control.Prompt, control.Texts, none.Prompt)
	}
	arm := runtime.Variant{Experiment: "shorter", Name: "control"}
	if want := append(slices.Clone(none.Variants), arm); !reflect.DeepEqual(control.Variants, want) {
		t.Errorf("the control arm's launch runs in %+v, want %+v: the arm names no overlay and no folder", control.Variants, want)
	}
	if _, err := os.Stat(filepath.Join(r.specs.stateDir, "texts")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the daemon copied a folder for a launch of the control arm (%v)", err)
	}
	if lines := loggedLines(t, r.logs.String(), "folder of texts"); len(lines) != 0 {
		t.Errorf("the control arm's launch logged %v, want nothing of a folder", lines)
	}
}

// Every cause the daemon's own reading and check of a folder can find has a row here, and each
// row's launch falls back whole: its spec is the launch of no arm of the experiment, field for
// field, with Legion's own parts and none of the arm's overlays or instructions while the arm of
// the other experiment stays; it carries no folder and the daemon keeps no copy; the daemon logs
// one line naming the claim, the generation, the experiment, the arm, the folder, the file and the
// cause, with the fingerprint it read the folder at; and the claim's issue is told once, by one
// fact. A cause the daemon cannot find is named here as the runtime's or the load's, and a cause
// that is neither and has no row fails by name.
func TestEveryCauseHasARowThatFallsBackWhole(t *testing.T) {
	t.Parallel()
	reviewer := map[string]string{"roles/reviewer.md": "The folder's reviewer.\n"}
	digests := func(t *testing.T, paths ...string) map[string]string {
		manifest := map[string]string{}
		for _, path := range paths {
			manifest[path] = legionDigest(t, "roles/reviewer.md")
		}
		return manifest
	}
	type row struct {
		roles []claim.Role
		// folder is the folder the arm names, and file the file its fault names, "" for none.
		folder func(t *testing.T) string
		file   string
		// unknownNames is a boot that read no names the image ships.
		unknownNames bool
	}
	rows := map[texts.Cause]row{
		texts.Gone: {folder: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }},
		texts.Unreadable: {folder: func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "texts")
			if err := os.WriteFile(file, []byte("a file where the folder stood\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return file
		}},
		texts.NoManifest: {folder: func(t *testing.T) string { return textsFolder(t, reviewer, nil) }, file: texts.ManifestName},
		texts.ManifestUnreadable: {folder: func(t *testing.T) string {
			return textsFolder(t, map[string]string{"roles/reviewer.md": "The folder's reviewer.\n", texts.ManifestName: "{"}, nil)
		}, file: texts.ManifestName},
		texts.Unlisted: {folder: func(t *testing.T) string {
			return textsFolder(t, map[string]string{"roles/reviewer.md": "The folder's reviewer.\n", "roles/tester.md": "The folder's tester.\n"},
				digests(t, "roles/reviewer.md"))
		}, file: "roles/tester.md"},
		texts.Missing: {folder: func(t *testing.T) string {
			return textsFolder(t, reviewer, digests(t, "roles/reviewer.md", "roles/tester.md"))
		}, file: "roles/tester.md"},
		texts.BadPath: {folder: func(t *testing.T) string {
			return textsFolder(t, map[string]string{"roles/reviewer.md": "The folder's reviewer.\n", "notes/reviewer.md": "A note.\n"},
				digests(t, "roles/reviewer.md", "notes/reviewer.md"))
		}, file: "notes/reviewer.md"},
		texts.LinkEscapes: {folder: func(t *testing.T) string {
			outside := filepath.Join(t.TempDir(), "reviewer.md")
			if err := os.WriteFile(outside, []byte("A reviewer outside the folder.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			folder := textsFolder(t, nil, digests(t, "roles/reviewer.md"))
			if err := os.MkdirAll(filepath.Join(folder, "roles"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(folder, "roles", "reviewer.md")); err != nil {
				t.Fatal(err)
			}
			return folder
		}, file: "roles/reviewer.md"},
		texts.DigestDiffers: {folder: func(t *testing.T) string {
			return textsFolder(t, reviewer, map[string]string{"roles/reviewer.md": legionDigest(t, "roles/tester.md")})
		}, file: "roles/reviewer.md"},
		texts.NoLegionText: {folder: func(t *testing.T) string {
			return textsFolder(t, map[string]string{"roles/no-such-part.md": "A part Legion lacks.\n"}, digests(t, "roles/no-such-part.md"))
		}, file: "roles/no-such-part.md"},
		texts.Unread: {roles: []claim.Role{claim.RoleReviewer}, folder: func(t *testing.T) string {
			return textsFolder(t, map[string]string{"roles/planner.md": "The folder's planner.\n"}, map[string]string{"roles/planner.md": legionDigest(t, "roles/planner.md")})
		}, file: "roles/planner.md"},
		texts.UnknownAgent: {folder: func(t *testing.T) string {
			return reviewerFolder(t, "Ask agent=\"no-such-agent\" for a second reading.\n")
		}, file: "roles/reviewer.md"},
		texts.UnknownSkill: {folder: func(t *testing.T) string {
			return reviewerFolder(t, "Read skill://no-such-skill before the diff.\n")
		}, file: "roles/reviewer.md"},
		texts.NamesUnread: {folder: func(t *testing.T) string {
			return reviewerFolder(t, "Ask agent=\"goal-red-team\" for a second reading.\n")
		}, unknownNames: true},
	}
	// The causes only a runtime finds, since only it reads what they are about, and the one only the
	// load finds.
	elsewhere := map[texts.Cause]string{
		texts.NameDiffers:         "the runtime's: the frontmatter of the folder's agents and skills is held to their paths where the plugins the launch loads are read (texts.CheckPlugins)",
		texts.LauncherCannotCheck: "the sandbox runtime's: a pod's launcher says at its hello whether it checks a folder",
		texts.InstallsPlugins:     "the sandbox runtime's: it decides whether a launch installs Legion's plugins after its start",
		texts.Changed:             "the launcher's: it reads the folder after the daemon did",
		texts.TwoFolders:          "the load's: two experiments that give one role a folder never reach a launch",
	}
	pool := isolatedOutboxPool(t)
	for n, cause := range texts.Causes {
		tc, ok := rows[cause]
		if !ok {
			if elsewhere[cause] == "" {
				t.Errorf("the cause %s has no row here and is named neither a runtime's nor the load's: add the row of a launch that meets it", cause)
			}
			continue
		}
		if elsewhere[cause] != "" {
			t.Errorf("the cause %s has a row here and is named %s too", cause, elsewhere[cause])
		}
		t.Run(string(cause), func(t *testing.T) {
			t.Parallel()
			tree := fmt.Sprintf("LEGION-%d", 100+n)
			seedTextsIssue(t, pool, tree)
			folder := tc.folder(t)
			r := newTextsRig(t, tree, tc.roles, folder)
			if tc.unknownNames {
				r.specs.shipped = nil
			}
			r.tellThrough(pool)
			token := mustClaimToken(t, tree, claim.RoleReviewer)

			control := r.spawn("", tree, claim.RoleReviewer, 4)
			spec := r.spawn("folder", tree, claim.RoleReviewer, 4)
			if spec.Texts != nil {
				t.Fatalf("the launch carries the folder %+v, want its fallback", spec.Texts)
			}
			if !reflect.DeepEqual(spec.Prompt, control.Prompt) || !reflect.DeepEqual(spec.Variants, control.Variants) {
				t.Errorf("the fallback reads %+v in the arms %+v, want the launch of no arm of the experiment: %+v in %+v",
					spec.Prompt, spec.Variants, control.Prompt, control.Variants)
			}
			longer := []runtime.Variant{{Experiment: "longer", Name: "on", ConfigFiles: []string{"/etc/legion/longer.yml"},
				ConfigFilesField: "projects.LEGION.experiments.longer.variants.on.config_files"}}
			if !reflect.DeepEqual(control.Variants, longer) || len(control.Prompt.VariantInstructionsPaths) != 1 ||
				!reflect.DeepEqual(control.Prompt.RolePromptPaths, r.legionParts(claim.RoleReviewer, true)) {
				t.Errorf("the launch of no arm of the experiment reads %+v in %+v, want Legion's parts, `longer`'s arm and its instructions alone",
					control.Prompt, control.Variants)
			}
			if prompt := composed(t, spec); strings.Contains(prompt, strings.TrimSpace(textsInstructions("shorter", "folder"))) ||
				!strings.Contains(prompt, strings.TrimSpace(textsInstructions("longer", "on"))) {
				t.Errorf("the fallback's prompt is %q, want `longer`'s instructions and none of the folder's arm", prompt)
			}
			if _, err := os.Stat(textsCopies(r.specs.stateDir, token)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the daemon keeps a copy of a folder its launch fell back from (%v)", err)
			}
			r.wantFallbackLine(token, 4, folder, tc.file, cause, fingerprintOf(t, folder))

			r.awaitNotices(token, 1)
			if got := textsFacts(t, pool, token); got != 1 {
				t.Errorf("intake applied %d fallback facts of %s, want one", got, token)
			}
			if messages := textsMessages(t, pool, tree); len(messages) != 1 || !strings.Contains(messages[0], strconv.Quote(string(cause))) {
				t.Errorf("the issue was told %q, want one message naming the cause %s", messages, cause)
			}
		})
	}
}

// A claim's launches that meet one cause tell its issue once: the second launch, at the next
// generation, falls back and logs as the first did, and its fact is one intake has applied
// already. A daemon started again over the same store tells nobody again either, since the
// record of the applied fact is the store's.
func TestTheSecondLaunchWithTheSameCausePostsNoSecondNotice(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	const tree = "LEGION-1"
	seedTextsIssue(t, pool, tree)
	folder := textsFolder(t, map[string]string{"roles/reviewer.md": "The folder's reviewer.\n"},
		map[string]string{"roles/reviewer.md": legionDigest(t, "roles/tester.md")})
	token := mustClaimToken(t, tree, claim.RoleReviewer)

	first := newTextsRig(t, tree, nil, folder)
	first.tellThrough(pool)
	for generation := uint64(1); generation <= 2; generation++ {
		if spec := first.spawn("folder", tree, claim.RoleReviewer, generation); spec.Texts != nil {
			t.Fatalf("launch %d carries the folder, want its fallback", generation)
		}
		first.awaitNotices(token, int(generation))
	}
	if got := len(first.lines(textsFallbackLine, token)); got != 2 {
		t.Errorf("the daemon logged %d fallbacks, want one for each launch", got)
	}
	if got := len(first.lines(textsNoticedAgainLine, token)); got != 1 {
		t.Errorf("the second launch's notice was applied already %d times, want once", got)
	}

	restarted := newTextsRig(t, tree, nil, folder)
	restarted.tellThrough(pool)
	restarted.spawn("folder", tree, claim.RoleReviewer, 3)
	restarted.awaitNotices(token, 1)
	if got := len(restarted.lines(textsNoticedAgainLine, token)); got != 1 {
		t.Errorf("the restarted daemon's launch applied %d notices already applied, want its one", got)
	}
	if messages := textsMessages(t, pool, tree); len(messages) != 1 {
		t.Errorf("the issue was told %q, want one message for the claim and its cause", messages)
	}
	if got := textsFacts(t, pool, token); got != 1 {
		t.Errorf("intake applied %d fallback facts of %s, want one", got, token)
	}
}

// A cause is the folder as it was read, the file and the reason: the same file failing for the
// same reason in a folder cut again is a new cause, and so is another reason, and each tells the
// issue once more.
func TestANewCausePostsANewNotice(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	const tree = "LEGION-1"
	seedTextsIssue(t, pool, tree)
	stale := map[string]string{"roles/reviewer.md": legionDigest(t, "roles/tester.md")}
	folder := textsFolder(t, map[string]string{"roles/reviewer.md": "The folder's reviewer.\n"}, stale)
	token := mustClaimToken(t, tree, claim.RoleReviewer)
	r := newTextsRig(t, tree, nil, folder)
	r.tellThrough(pool)

	launch := func(generation uint64) {
		t.Helper()
		if spec := r.spawn("folder", tree, claim.RoleReviewer, generation); spec.Texts != nil {
			t.Fatalf("launch %d carries the folder, want its fallback", generation)
		}
		r.awaitNotices(token, int(generation))
	}
	launch(1)
	// The folder cut again, its reviewer edited and still recorded at another text's digest.
	if err := os.WriteFile(filepath.Join(folder, "roles", "reviewer.md"), []byte("The folder's reviewer, cut again.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	launch(2)
	// Another reason: the manifest gone.
	if err := os.Remove(filepath.Join(folder, texts.ManifestName)); err != nil {
		t.Fatal(err)
	}
	launch(3)

	messages := textsMessages(t, pool, tree)
	if len(messages) != 3 || len(r.lines(textsNoticedLine, token)) != 3 {
		t.Fatalf("the issue was told %q, want one message for each of the three causes", messages)
	}
	for i, cause := range []texts.Cause{texts.DigestDiffers, texts.DigestDiffers, texts.NoManifest} {
		if !strings.Contains(messages[i], strconv.Quote(string(cause))) {
			t.Errorf("message %d is %q, want it to name the cause %s", i, messages[i], cause)
		}
	}
}

// A claim whose role prompt is the operator's own file (`<state>/prompts/<token>.md`) keeps that
// file as its whole role prompt, and still reads the folder for its agents and skills: the launch
// carries the folder, and the daemon's copy holds the folder's agent and skill as it read them, so
// the root a runtime builds from the copy replaces both of the folder's skills.
func TestAnOperatorPromptFileKeepsItsTextAndTakesTheFoldersAgents(t *testing.T) {
	t.Parallel()
	agent := "---\nname: goal-red-team\ndescription: The folder's red team.\n---\nRead the change against the goal.\n"
	folder := textsFolder(t, map[string]string{
		"roles/reviewer.md":              "The folder's reviewer.\n",
		"agents/goal-red-team.md":        agent,
		"skills/legion-worker/SKILL.md":  "---\nname: legion-worker\ndescription: The folder's worker skill.\n---\nWork.\n",
		"skills/legion-worker/notes.md":  "A note of the skill.\n",
		"skills/dispatch-first/SKILL.md": "---\nname: dispatch-first\ndescription: The folder's dispatch-first.\n---\nSearch first.\n",
	}, map[string]string{
		"roles/reviewer.md":              legionDigest(t, "roles/reviewer.md"),
		"agents/goal-red-team.md":        texts.Digest([]byte("Legion's red team.\n")),
		"skills/legion-worker/SKILL.md":  texts.Digest([]byte("Legion's worker skill.\n")),
		"skills/legion-worker/notes.md":  texts.Digest([]byte("Legion's note.\n")),
		"skills/dispatch-first/SKILL.md": texts.Digest([]byte("Legion's dispatch-first.\n")),
	})
	r := newTextsRig(t, "LEGION-1", nil, folder)
	token := mustClaimToken(t, "LEGION-2", claim.RoleReviewer)
	operator := rolePromptPath(r.specs.stateDir, token)
	if err := os.MkdirAll(filepath.Dir(operator), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operator, []byte("The operator's own prompt for this reviewer.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 5)
	if !reflect.DeepEqual(spec.Prompt.RolePromptPaths, []string{operator}) {
		t.Errorf("the launch reads %q, want the operator's file alone", spec.Prompt.RolePromptPaths)
	}
	if prompt := composed(t, spec); !strings.Contains(prompt, "The operator's own prompt") || strings.Contains(prompt, "The folder's reviewer.") {
		t.Errorf("the prompt is %q, want the operator's text and not the folder's role part", prompt)
	}
	if spec.Texts == nil {
		t.Fatalf("the launch carries no folder, want its agents and skills; logged %s", r.logs.String())
	}
	copied, err := texts.Read(spec.Texts.Copy)
	if err != nil {
		t.Fatalf("texts.Read of the daemon's copy = %v", err)
	}
	plugins := t.TempDir()
	envoy, legion := filepath.Join(plugins, "pi-envoy"), filepath.Join(plugins, "pi-legion")
	writeTree(t, legion, map[string]string{
		filepath.Join("agents", "goal-red-team.md"):                  "Legion's red team.\n",
		filepath.Join("dist", "skills", "legion-worker", "SKILL.md"): "Legion's worker skill.\n",
		filepath.Join("dist", "skills", "legion-worker", "notes.md"): "Legion's note.\n",
	})
	writeTree(t, envoy, map[string]string{filepath.Join("dist", "skills", "dispatch-first", "SKILL.md"): "Legion's dispatch-first.\n"})
	if root, err := texts.BuildRoot(copied, envoy, legion, filepath.Join(plugins, "root")); err != nil || !reflect.DeepEqual(root.Touched, []string{"dispatch-first", "legion-worker"}) {
		t.Errorf("the root built from the copy replaces the skills %q (%v), want dispatch-first and legion-worker", root.Touched, err)
	}
	if got, err := os.ReadFile(filepath.Join(spec.Texts.Copy, "agents", "goal-red-team.md")); err != nil || string(got) != agent {
		t.Errorf("the copy's agent is %q (%v), want the folder's", got, err)
	}
	if _, err := os.Stat(filepath.Join(spec.Texts.Copy, "skills", "dispatch-first", "SKILL.md")); err != nil {
		t.Errorf("the copy holds no dispatch-first: %v", err)
	}
}

// Each launch reads the folder as it stands then: an edit between two launches reaches the second,
// whose copy is a new one, and the first launch's copy is gone.
func TestTheLaunchReadsTheFolderAsItStandsThen(t *testing.T) {
	t.Parallel()
	folder := reviewerFolder(t, "The folder's first reviewer.\n")
	r := newTextsRig(t, "LEGION-1", nil, folder)
	first := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 1)
	if first.Texts == nil || !strings.Contains(composed(t, first), "The folder's first reviewer.") {
		t.Fatalf("the first launch carries %+v, want the folder's first text; logged %s", first.Texts, r.logs.String())
	}
	if err := os.WriteFile(filepath.Join(folder, "roles", "reviewer.md"), []byte("The folder's second reviewer.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 2)
	if second.Texts == nil {
		t.Fatalf("the second launch carries no folder; logged %s", r.logs.String())
	}
	if prompt := composed(t, second); !strings.Contains(prompt, "The folder's second reviewer.") || strings.Contains(prompt, "first reviewer") {
		t.Errorf("the second launch's prompt is %q, want the folder's text as it stands at the second launch", prompt)
	}
	if second.Texts.Fingerprint == first.Texts.Fingerprint || second.Texts.Copy == first.Texts.Copy {
		t.Errorf("the second launch read %+v, want a fingerprint and a copy of its own beside the first's %+v", second.Texts, first.Texts)
	}
	if _, err := os.Stat(filepath.Dir(first.Texts.Copy)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the first launch's copy outlives the second launch (%v)", err)
	}
}

// A launch removes the copies the claim's earlier launches made and none of their roots: a process
// of the claim may still load an earlier generation's root, and only the runtime knows, once its
// refusal of a live claim has passed, that none does (tmux's checkedTexts.build). A launch at a
// generation whose copy is already there writes that copy anew, so a file the folder no longer
// holds is not left in it.
func TestALaunchRemovesTheEarlierCopiesAndLeavesTheirRoots(t *testing.T) {
	t.Parallel()
	folder := reviewerFolder(t, "The folder's reviewer.\n")
	r := newTextsRig(t, "LEGION-1", nil, folder)
	first := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 1)
	if first.Texts == nil {
		t.Fatalf("the first launch carries no folder; logged %s", r.logs.String())
	}
	const loaded = "A root a live pane of the claim loads.\n"
	root := filepath.Join(filepath.Dir(first.Texts.Copy), "root")
	writeTree(t, root, map[string]string{filepath.Join("agents", "goal-red-team.md"): loaded})

	second := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 2)
	if second.Texts == nil {
		t.Fatalf("the second launch carries no folder; logged %s", r.logs.String())
	}
	if _, err := os.Stat(first.Texts.Copy); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the first launch's copy outlives the second launch (%v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "agents", "goal-red-team.md")); err != nil || string(got) != loaded {
		t.Errorf("the second launch left the first launch's root holding %q (%v), want it untouched", got, err)
	}

	stale := filepath.Join(second.Texts.Copy, "roles", "tester.md")
	writeTree(t, filepath.Dir(stale), map[string]string{"tester.md": "A file the folder does not hold.\n"})
	again := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 2)
	if again.Texts == nil || again.Texts.Copy != second.Texts.Copy {
		t.Fatalf("the launch again at generation 2 carries %+v, want the copy %s", again.Texts, second.Texts.Copy)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the launch again at generation 2 kept %s, which the folder does not hold (%v)", stale, err)
	}
	if _, err := os.Stat(filepath.Join(again.Texts.Copy, "roles", "reviewer.md")); err != nil {
		t.Errorf("the launch again at generation 2 holds no copy of the folder's reviewer: %v", err)
	}
}

// The prompt a runtime composes after the launch's build is the folder as the daemon read and
// checked it: a folder changed after the check, and one gone by then, change nothing of it.
func TestTheCopyIsWhatThePromptIsComposedFrom(t *testing.T) {
	t.Parallel()
	folder := reviewerFolder(t, "The reviewer the daemon checked.\n")
	r := newTextsRig(t, "LEGION-1", nil, folder)
	spec := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 1)
	if spec.Texts == nil {
		t.Fatalf("the launch carries no folder; logged %s", r.logs.String())
	}
	if err := os.WriteFile(filepath.Join(folder, "roles", "reviewer.md"), []byte("A reviewer written after the check.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if prompt := composed(t, spec); !strings.Contains(prompt, "The reviewer the daemon checked.") || strings.Contains(prompt, "after the check") {
		t.Errorf("the prompt composed after the folder changed is %q, want the bytes the daemon checked", prompt)
	}
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	if prompt := composed(t, spec); !strings.Contains(prompt, "The reviewer the daemon checked.") {
		t.Errorf("the prompt composed after the folder went is %q, want the bytes the daemon checked", prompt)
	}
}

// An arm that names a folder and no other file is one a unit runs with: the record of the tree's
// draw is read for it (experiment.Launch.Keys), and its launch reads the folder.
func TestAFolderOnlyArmIsReadAtLaunch(t *testing.T) {
	t.Parallel()
	const tree = "LEGION-1"
	folder := reviewerFolder(t, "The folder-only arm's reviewer.\n")
	declared := []config.Experiment{{Name: "shorter", Mode: config.ExperimentRandom, Unit: config.ExperimentTree, Variants: []config.Variant{
		{Name: "folder", Weight: 1, TextsPath: folder}, {Name: "control", Weight: 1}}}}
	r := newTextsRigOf(t, tree, declared)
	if keys := experiment.ForLaunch(declared, r.specs.stateDir).Keys(tree, tree); !reflect.DeepEqual(keys, []string{tree}) {
		t.Fatalf("a launch reads the record of %v, want the tree's: an arm that names a folder alone names a file", keys)
	}
	pool := isolatedOutboxPool(t)
	seedTextsIssue(t, pool, tree)
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := record.NewStore().PutAssignments(ctx, tx, tree, []record.Assignment{{Experiment: "shorter", Unit: "tree", Variant: "folder", Mode: "random"}})
		return err
	}); err != nil {
		t.Fatalf("record the draw: %v", err)
	}
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	s := r.specs
	s.variants = launchVariants(st, declared, r.specs.stateDir)
	spec := r.spawnWith(s, tree, claim.RoleReviewer, 1)
	if want := []runtime.Variant{{Experiment: "shorter", Name: "folder", Texts: folder}}; !reflect.DeepEqual(spec.Variants, want) || spec.Texts == nil {
		t.Fatalf("the launch runs in %+v with the folder %+v, want the arm %+v and its folder; logged %s", spec.Variants, spec.Texts, want, r.logs.String())
	}
	if prompt := composed(t, spec); !strings.Contains(prompt, "The folder-only arm's reviewer.") {
		t.Errorf("the prompt is %q, want the folder's reviewer", prompt)
	}
}

// A launch that reads its arm's folder records what it read: one line naming the claim, the
// generation, the experiment, the arm, the folder, its fingerprint and the daemon's copy, and each
// file the folder held, the manifest among them, with its digest.
func TestTheLaunchLogsWhatItRead(t *testing.T) {
	t.Parallel()
	folder := reviewerFolder(t, "The folder's reviewer.\n")
	r := newTextsRig(t, "LEGION-1", nil, folder)
	spec := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 7)
	if spec.Texts == nil {
		t.Fatalf("the launch carries no folder; logged %s", r.logs.String())
	}
	token := mustClaimToken(t, "LEGION-2", claim.RoleReviewer)
	lines := r.lines(textsReadLine, token)
	if len(lines) != 1 {
		t.Fatalf("the daemon logged %d reads of %s, want one: %s", len(lines), token, r.logs.String())
	}
	read, err := texts.Read(folder)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]any{}
	for path, body := range read.Files {
		files[path] = texts.Digest(body)
	}
	want := map[string]any{"level": "INFO", "generation": float64(7), "experiment": "shorter", "arm": "folder", "folder": folder,
		"fingerprint": read.Fingerprint, "copy": spec.Texts.Copy, "files": files}
	for key, value := range want {
		if !reflect.DeepEqual(lines[0][key], value) {
			t.Errorf("the read line's %s = %v, want %v", key, lines[0][key], value)
		}
	}
	if len(files) != 2 {
		t.Errorf("the folder holds %v, want its reviewer and its manifest", files)
	}
}

// A folder whose texts name an agent or a skill the worker image does not ship falls back, with
// the name read from the image as either runtime's boot reads it: the plugin gate's answer under
// tmux, and the image probe's log under kubernetes, whose shipped line the probe report carries. A
// boot that read no names, from an image whose probe predates the line, falls back for any name
// with names-unread, and names the image ships pass.
func TestAFolderNamingAnAgentTheImageDoesNotShipFallsBackOnEitherRuntime(t *testing.T) {
	t.Parallel()
	agents, skills := shippedNamesFixture(t)
	gate := bootprobe.NewShippedNames(agents, skills)
	tail := strings.Join([]string{"pi.agents: ok", bootprobe.ShippedLine(gate)}, "\n")
	probed, ok := bootprobe.Shipped(tail)
	if !ok {
		t.Fatalf("the probe's log tail %q carries no shipped line", tail)
	}
	report := bootprobe.ImageReport{Shipped: &probed}
	for _, rt := range []struct {
		name, from string
		names      *bootprobe.ShippedNames
	}{
		{"tmux", "the plugin gate", &gate},
		{"kubernetes", "the image probe", report.Shipped},
	} {
		for _, tc := range []struct {
			name    string
			text    string
			cause   texts.Cause
			noNames bool
		}{
			{"an agent the image does not ship", "Ask agent=\"no-such-agent\" for a second reading.\n", texts.UnknownAgent, false},
			{"a skill the image does not ship", "Read skill://no-such-skill before the diff.\n", texts.UnknownSkill, false},
			{"a boot that read no names", "Ask agent=\"goal-red-team\" for a second reading.\n", texts.NamesUnread, true},
			{"names the image ships", "Ask agent=\"goal-red-team\", then read skill://legion-worker.\n", "", false},
		} {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				folder := reviewerFolder(t, tc.text)
				r := newTextsRig(t, "LEGION-1", nil, folder)
				holder := &shippedNames{}
				names := rt.names
				if tc.noNames {
					names = nil
				}
				holder.bind(names, rt.from, quietLogger())
				r.specs.shipped = holder.read
				spec := r.spawn("folder", "LEGION-2", claim.RoleReviewer, 2)
				token := mustClaimToken(t, "LEGION-2", claim.RoleReviewer)
				if tc.cause == "" {
					if spec.Texts == nil {
						t.Fatalf("the launch fell back from a folder naming what the image ships: %s", r.logs.String())
					}
					return
				}
				if spec.Texts != nil {
					t.Fatalf("the launch carries the folder, want its fallback for %s", tc.cause)
				}
				file := "roles/reviewer.md"
				if tc.cause == texts.NamesUnread {
					file = ""
				}
				r.wantFallbackLine(token, 2, folder, file, tc.cause, fingerprintOf(t, folder))
			})
		}
	}
}
