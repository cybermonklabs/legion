//go:build e2e && !textscontrol

// The arm of texts_folder_live_test.go's experiment as this checkout runs it: a folder of texts.
// Every use of the key `texts`, of the texts package and of the other symbols the folder of texts
// added to the daemon is in this file, so the driver beside it builds over a tree of main with
// texts_folder_arm_control_live_test.go in this file's place (scripts/e2e/texts-folder.sh
// --control). Each function here has the signature its counterpart there has.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ompdirs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// folderProofShippedLine is the boot's line of the agents and skills the worker image ships, and
// folderProofNoShippedLine its line for a boot that read none (daemon.go's shippedNames.bind).
const (
	folderProofShippedLine   = "legion daemon read the agents and skills the worker image ships"
	folderProofNoShippedLine = "legion daemon read no agent or skill names the worker image ships; a launch of an arm's folder of texts that names an agent or a skill falls back (names-unread)"
)

// folderProofArm builds the case's folder of texts from the checkout's own texts and answers the
// arm that names it. Each role part is the daemon's own text (prompts.Texts, the checkout's
// internal/prompts as this driver embeds it): roles/architect-root.md, roles/architect.md,
// go/architect-common.md and go/reviewer.md each with its marker after it, and roles/reviewer.md
// with folderProofRemoved taken out and its marker after it. agents/goal-red-team.md is pi-legion's
// as the proof's profile installed it, with a marker opening its description;
// skills/legion-worker/references/plan-tests.md the installed plugin's with a marker under its
// heading, and no SKILL.md beside it; skills/dispatch-first/SKILL.md the installed plugin's with a
// marker after it. The manifest records, for each file, the digest of Legion's text at its path:
// the daemon's part for a role part, and the installed plugin's file for an agent or a skill, which
// is what the launch checks each against. A fallback case changes the folder as it says: the
// recorded digest of roles/reviewer.md by one character (digest), roles/no-such-part.md listed
// (no-legion-text), a task call's agent `agent="no-such-agent"` in goal-red-team's body
// (unknown-agent), or `skill://no-such-skill` in go/reviewer.md (unknown-skill). The folder is read
// with texts.Read once it is written, so a folder the proof cut wrong fails here and not at a
// launch.
func folderProofArm(r *folderProofLive) config.Variant {
	t := r.t
	t.Helper()
	dir := filepath.Join(t.TempDir(), "texts-folder")
	envoyRoot, legionRoot := folderProofPlugins(r)
	m := r.markers
	files, digests := map[string]string{}, map[string]string{}
	part := func(path string, edit func(string) string) {
		body, err := fs.ReadFile(prompts.Texts(), path)
		if err != nil {
			t.Fatalf("read Legion's %s: %v", path, err)
		}
		files[path], digests[path] = edit(string(body)), texts.Digest(body)
	}
	plugin := func(path string, edit func(string) string) {
		var file string
		switch {
		case strings.HasPrefix(path, "agents/"):
			file = filepath.Join(legionRoot, filepath.FromSlash(path))
		default:
			rest := strings.TrimPrefix(path, "skills/")
			file = filepath.Join(legionRoot, "dist", "skills", filepath.FromSlash(rest))
			if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
				file = filepath.Join(envoyRoot, "dist", "skills", filepath.FromSlash(rest))
			}
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read the installed plugin's %s, Legion's text at %s: %v", file, path, err)
		}
		files[path], digests[path] = edit(string(body)), texts.Digest(body)
	}
	mark := func(marker string) func(string) string {
		return func(text string) string { return folderProofAppend(text, marker) }
	}
	part("roles/architect-root.md", mark(m.architectRoot))
	part("roles/architect.md", mark(m.architect))
	part("go/architect-common.md", mark(m.architectCommon))
	part("go/reviewer.md", mark(m.reviewerGo))
	part("roles/reviewer.md", func(text string) string { return folderProofReviewer(t, text, m.reviewerRole) })
	plugin("agents/goal-red-team.md", func(text string) string { return folderProofDescribed(t, text, m.agent) })
	plugin("skills/legion-worker/references/plan-tests.md", func(text string) string {
		heading, rest, _ := strings.Cut(text, "\n")
		return heading + "\n\n" + m.reference + "\n" + rest
	})
	plugin("skills/dispatch-first/SKILL.md", mark(m.dispatchFirst))

	switch r.c.name {
	case "digest":
		recorded := digests["roles/reviewer.md"]
		flipped := "0"
		if strings.HasSuffix(recorded, "0") {
			flipped = "1"
		}
		digests["roles/reviewer.md"] = recorded[:len(recorded)-1] + flipped
	case "no-legion-text":
		files["roles/no-such-part.md"] = "A role part at a path where Legion has no text.\n"
		digests["roles/no-such-part.md"] = texts.Digest([]byte(files["roles/no-such-part.md"]))
	case "unknown-agent":
		files["agents/goal-red-team.md"] += "\nRun `task(agent=\"no-such-agent\")` before you report.\n"
	case "unknown-skill":
		files["go/reviewer.md"] += "\nRead `skill://no-such-skill` before you review.\n"
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		file := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		writeProofFile(t, file, files[path])
	}
	if err := texts.WriteManifest(dir, digests); err != nil {
		t.Fatalf("write the folder's manifest: %v", err)
	}
	folder, err := texts.Read(dir)
	if err != nil {
		t.Fatalf("the folder the proof cut does not read: %v", err)
	}
	r.folder = dir
	r.note("the arm %s names the folder %s, fingerprint %s: %s", folderProofArmName, dir, folder.Fingerprint, strings.Join(slices.Sorted(maps.Keys(files)), ", "))
	return config.Variant{Name: folderProofArmName, Weight: 1, TextsPath: dir}
}

// folderProofPlugins are the pi-envoy and pi-legion roots the proof's panes load, found as the
// local runtime's launch finds them (tmux.PrepareTexts): the installed plugins of the profile, over
// the environment the runtime prepares a launch under.
func folderProofPlugins(r *folderProofLive) (envoyRoot, legionRoot string) {
	r.t.Helper()
	env := tmux.PaneEnvironment([]string{"HOME=" + r.shared.home, "OMP_PROFILE=" + r.shared.profile}, r.cfg.StateDir)
	root, _, err := ompdirs.ProfileRoot(env, r.cfg.StateDir)
	if err != nil {
		r.t.Fatalf("resolve the profile root of %s: %v", r.shared.profile, err)
	}
	plugins := filepath.Join(root, "plugins", "node_modules", "@sjawhar")
	return filepath.Join(plugins, "pi-envoy"), filepath.Join(plugins, "pi-legion")
}

// folderProofDescribed is the agent text with marker opening its frontmatter description, so the
// task tool's agent list shows it whatever it cuts of the rest.
func folderProofDescribed(t *testing.T, text, marker string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		value, found := strings.CutPrefix(line, "description:")
		if !found {
			continue
		}
		switch value = strings.TrimSpace(value); {
		case value == "|" || value == ">" || strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">"):
			lines = slices.Insert(lines, i+1, "  "+marker+".")
		case strings.HasPrefix(value, `"`):
			lines[i] = `description: "` + marker + ". " + strings.TrimPrefix(value, `"`)
		default:
			lines[i] = "description: " + marker + ". " + value
		}
		return strings.Join(lines, "\n")
	}
	t.Fatalf("the agent text holds no frontmatter description to put the marker in:\n%s", text)
	return ""
}

// folderProofOverrides gives the boot the plugin gate a production boot under tmux runs, which a
// replaced runtime runs none of (daemon.go, overrides.gate): the real pane-lane gate over the
// proof's own profile, which sets LEGION_PROMPT_LIST itself, so the boot reads the agents and
// skills the profile ships as a production boot does. It skips the shipped agents' models, as the
// plugin-pack check's gate does, since the profile configures none of the roles they name. It also
// has the local runtime tell the proof each launch whose folder failed the check against the
// installed plugins.
func folderProofOverrides(r *folderProofLive, o *overrides) {
	gate := pluginGate{
		env: map[string]string{
			"HOME": r.shared.home, "OMP_PROFILE": r.shared.profile, "PATH": os.Getenv("PATH"), "AWS_EC2_METADATA_DISABLED": "true",
		},
		workDir: r.cfg.StateDir, invocation: r.omp, timeout: defaultProbeTimeout, retry: bootprobe.Image,
		contract: api.DaemonAPIVersion(), skipAgentModels: true, log: slog.New(r.daemon),
	}
	o.gate = func(ctx context.Context) (*bootprobe.ShippedNames, error) { return gate.verify(ctx) }
	r.rt.textsFellBack = func(report runtime.TextsReport) {
		r.runtimeFellBack(string(report.Claim), string(report.Fault.Cause), report.Fault.Path, report.Fault.Detail)
	}
}

// folderProofBootRead holds the boot's line of the agents and skills the gate read, from the
// plugin gate with at least one of each, and holds that it came before any launch of the folder,
// so a boot that read no names fails here rather than at a marker.
func folderProofBootRead(r *folderProofLive) {
	r.t.Helper()
	at := r.logAt(func(record loggedRecord) bool { return record.Message == folderProofShippedLine })
	if at < 0 {
		if r.logAt(func(record loggedRecord) bool { return record.Message == folderProofNoShippedLine }) >= 0 {
			r.fail("the boot read no agent or skill names the worker image ships, so every launch of the folder would fall back with names-unread")
		}
		r.fail("the boot logged no line %q", folderProofShippedLine)
	}
	line := r.logs(func(record loggedRecord) bool { return record.Message == folderProofShippedLine })[0]
	agents, _ := line.Attrs["agents"].(float64)
	skills, _ := line.Attrs["skills"].(float64)
	if line.Attrs["from"] != "the plugin gate" || agents < 1 || skills < 1 {
		r.fail("the boot's line of shipped names reads %v; want from the plugin gate, with agents and skills", line.Attrs)
	}
	launched := r.logAt(func(record loggedRecord) bool {
		return record.Message == folderProofLaunched || record.Message == folderProofFellBack
	})
	if launched >= 0 && launched < at {
		r.fail("a launch of the folder was logged before the boot's line of shipped names")
	}
	r.note("the boot read %v agents and %v skills from the plugin gate before any launch", agents, skills)
}

// folderProofBreak changes the folder on disk, after a launch accepted it, as the case says: its
// manifest deleted, its mode set to 000, or roles/planner.md added to it and its manifest with the
// digest of Legion's text, a part no role the experiment reaches reads.
func folderProofBreak(r *folderProofLive) {
	t := r.t
	t.Helper()
	switch r.c.name {
	case "manifest-deleted":
		if err := os.Remove(filepath.Join(r.folder, texts.ManifestName)); err != nil {
			t.Fatalf("delete the folder's manifest: %v", err)
		}
		r.note("deleted %s", filepath.Join(r.folder, texts.ManifestName))
	case "unreadable":
		if os.Geteuid() == 0 {
			r.fail("this run is root, which reads a folder of mode 000, so the case cannot make the folder unreadable")
		}
		if err := os.Chmod(r.folder, 0); err != nil {
			t.Fatalf("set the folder's mode to 000: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(r.folder, 0o755) })
		if _, err := os.ReadDir(r.folder); err == nil {
			r.fail("the folder %s reads at mode 000", r.folder)
		}
		r.note("set the mode of %s to 000", r.folder)
	case "unreached-part":
		body, err := fs.ReadFile(prompts.Texts(), "roles/planner.md")
		if err != nil {
			t.Fatalf("read Legion's roles/planner.md: %v", err)
		}
		writeProofFile(t, filepath.Join(r.folder, "roles", "planner.md"), string(body))
		raw, err := os.ReadFile(filepath.Join(r.folder, texts.ManifestName))
		if err != nil {
			t.Fatalf("read the folder's manifest: %v", err)
		}
		var manifest texts.Manifest
		if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&manifest); err != nil {
			t.Fatalf("decode the folder's manifest: %v", err)
		}
		manifest.Files["roles/planner.md"] = texts.Digest(body)
		if err := texts.WriteManifest(r.folder, manifest.Files); err != nil {
			t.Fatalf("write the folder's manifest: %v", err)
		}
		r.note("added roles/planner.md, Legion's text, to %s and its manifest", r.folder)
	default:
		t.Fatalf("the case %s changes no folder after its start", r.c.name)
	}
}
