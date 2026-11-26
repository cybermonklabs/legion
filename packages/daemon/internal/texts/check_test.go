package texts_test

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// read is Read of dir, failing the test where it refuses.
func read(t *testing.T, dir string) texts.Folder {
	t.Helper()
	folder, err := texts.Read(dir)
	if err != nil {
		t.Fatalf("Read(%s) = %v, want it read", dir, err)
	}
	return folder
}

// recordDigests rewrites dir's manifest so that each path of digests records that digest.
func recordDigests(t *testing.T, dir string, digests map[string]string) {
	t.Helper()
	folder := read(t, dir)
	for p, digest := range digests {
		folder.Manifest.Files[p] = digest
	}
	if err := texts.WriteManifest(dir, folder.Manifest.Files); err != nil {
		t.Fatal(err)
	}
}

// faultsByPath is the cause of each fault, by the path it names, and fails for a fault that names
// another folder or a path another fault names.
func faultsByPath(t *testing.T, dir string, faults []texts.Fault) map[string]texts.Cause {
	t.Helper()
	got := map[string]texts.Cause{}
	for _, f := range faults {
		if _, twice := got[f.Path]; twice {
			t.Errorf("two faults name %s: %v", f.Path, faults)
		}
		if f.Dir != dir {
			t.Errorf("the fault %+v names the folder %q, want %s", f, f.Dir, dir)
		}
		got[f.Path] = f.Cause
	}
	return got
}

// Each role part of a folder is held to Legion's embedded part at its path: a path where Legion
// has no text (a file, or a directory of parts), a recorded digest that is not that of Legion's
// text, and a part no role the experiment reaches reads are each named, at the file, and a part
// that records Legion's digest and is read is not, whatever its own bytes.
func TestCheckRolePartsNamesEachFault(t *testing.T) {
	legion := fstest.MapFS{}
	for _, p := range []string{"roles/reviewer.md", "roles/core/common.md", "roles/mechanics/headless.md", "go/reviewer.md", "go/worker-common.md"} {
		legion[p] = &fstest.MapFile{Data: []byte(legionText(p))}
	}
	dir := writeFolder(t,
		"roles/reviewer.md",       // as it should be
		"roles/core/common.md",    // a stale digest
		"roles/no-such-part.md",   // no Legion text
		"roles/mechanics",         // a directory of Legion's parts, not a text
		"go/reviewer.md",          // read by no reached role
		"go/worker-common.md",     // a stale digest and read by no reached role: the digest is named
		"agents/goal-red-team.md", // not a role part
		"skills/legion-worker/SKILL.md")
	recordDigests(t, dir, map[string]string{
		"roles/core/common.md": texts.Digest([]byte("Legion's text of another version.\n")),
		"go/worker-common.md":  texts.Digest([]byte("Legion's text of another version.\n")),
	})
	reached := map[string]bool{"roles/reviewer.md": true, "roles/core/common.md": true, "roles/no-such-part.md": true, "roles/mechanics": true}
	faults := texts.CheckRoleParts(read(t, dir), legion, func(p string) bool { return reached[p] })

	want := map[string]texts.Cause{
		"roles/core/common.md":  texts.DigestDiffers,
		"roles/no-such-part.md": texts.NoLegionText,
		"roles/mechanics":       texts.NoLegionText,
		"go/reviewer.md":        texts.Unread,
		"go/worker-common.md":   texts.DigestDiffers,
	}
	if got := faultsByPath(t, dir, faults); !maps.Equal(got, want) {
		t.Errorf("CheckRoleParts named %v, want exactly %v", got, want)
	}
	for _, f := range faults {
		if f.Cause == texts.DigestDiffers && (!strings.Contains(f.Detail, texts.Digest([]byte(legionText(f.Path)))) ||
			!strings.Contains(f.Detail, texts.Digest([]byte("Legion's text of another version.\n")))) {
			t.Errorf("the digest fault %q does not name both the recorded digest and Legion's", f.Error())
		}
	}
	var paths []string
	for _, f := range faults {
		paths = append(paths, f.Path)
	}
	if !slices.IsSorted(paths) {
		t.Errorf("CheckRoleParts names %q, want them in the order of their paths", paths)
	}
}

// plugins lays out the two plugins as a launch loads them, each skill and agent holding Legion's
// text at its folder path (legionText), with frontmatter that names it as its path does, and
// returns the roots of pi-envoy and pi-legion.
func plugins(t *testing.T) (envoy, legion string) {
	t.Helper()
	base := t.TempDir()
	envoy, legion = filepath.Join(base, "pi-envoy"), filepath.Join(base, "pi-legion")
	for root, files := range map[string][]string{
		envoy: {"skills/dispatch-first/SKILL.md", "skills/asd-ste100/SKILL.md", "skills/asd-ste100/LICENSE", "skills/envoy/references/tools.md"},
		legion: {
			"agents/goal-red-team.md", "agents/oracle.md", "agents/deep-worker.md",
			"skills/legion-worker/SKILL.md", "skills/legion-worker/references/plan-tests.md", "skills/legion-retro/SKILL.md",
		},
	} {
		for _, p := range files {
			on := filepath.Join(root, filepath.FromSlash(p))
			if rest, ok := strings.CutPrefix(p, "skills/"); ok {
				on = filepath.Join(root, "dist", "skills", filepath.FromSlash(rest))
			}
			writeFile(t, on, legionText(p))
		}
	}
	return envoy, legion
}

// Each agent and skill file of a folder is held to the plugin file the launch loads: a recorded
// digest that is not that of the plugin's file (an agent, a pi-legion skill's reference, a
// pi-envoy skill), a path neither plugin ships (an agent, a skill, the skills/ guide, a skill's
// licence, which the pack ships but is no text), and a folder agent or SKILL.md whose frontmatter
// name is not the one its path gives (an agent with another name or none, a skill with another
// name) are each named at the file. An agent and a skill of either plugin that record the plugin
// file's digest and keep their names are not, nor is a SKILL.md that gives no name, since Oh My
// Pi names that skill by its directory. The plugins are given through links, as an installed
// plugin is.
func TestCheckPluginsNamesEachFault(t *testing.T) {
	envoy, legion := plugins(t)
	links := t.TempDir()
	for name, root := range map[string]string{"envoy": envoy, "legion": legion} {
		if err := os.Symlink(root, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	stale := texts.Digest([]byte("A plugin text of another release.\n"))
	frontmatter := func(name string) string {
		return "---\nname: " + name + "\ndescription: The arm's.\n---\n\nThe arm's body.\n"
	}
	for _, tc := range []struct {
		name, path, body string
		// digest is what the manifest records: "" is the plugin file's.
		digest string
		want   texts.Cause
	}{
		{"an agent as it should be", "agents/goal-red-team.md", frontmatter("goal-red-team"), "", ""},
		{"a pi-envoy skill as it should be", "skills/dispatch-first/SKILL.md", frontmatter("dispatch-first"), "", ""},
		{"a pi-legion skill's reference as it should be", "skills/legion-worker/references/plan-tests.md", "The arm's reference.\n", "", ""},
		{"a SKILL.md that gives no name", "skills/legion-retro/SKILL.md", "---\ndescription: The arm's.\n---\nBody.\n", "", ""},
		{"an agent's stale digest", "agents/oracle.md", frontmatter("oracle"), stale, texts.DigestDiffers},
		{"a pi-legion skill's stale digest", "skills/legion-worker/references/plan-tests.md", "The arm's reference.\n", stale, texts.DigestDiffers},
		{"a pi-envoy skill's stale digest", "skills/envoy/references/tools.md", "The arm's tools.\n", stale, texts.DigestDiffers},
		{"an agent neither plugin ships", "agents/new-agent.md", frontmatter("new-agent"), "", texts.NoLegionText},
		{"a skill neither plugin ships", "skills/no-such-skill/SKILL.md", frontmatter("no-such-skill"), "", texts.NoLegionText},
		{"a file a shipped skill lacks", "skills/legion-worker/references/new.md", "New.\n", "", texts.NoLegionText},
		{"the skills guide", "skills/AGENTS.md", "A guide.\n", "", texts.NoLegionText},
		{"a skill's licence", "skills/asd-ste100/LICENSE", "Another licence.\n", "", texts.NoLegionText},
		{"a skill's directory", "skills/legion-worker/references", "Not a directory.\n", "", texts.NoLegionText},
		{"an agent named otherwise", "agents/deep-worker.md", frontmatter("deep-worker-2"), "", texts.NameDiffers},
		{"an agent that gives no name", "agents/deep-worker.md", "No frontmatter at all.\n", "", texts.NameDiffers},
		{"an empty agent", "agents/deep-worker.md", "", "", texts.NameDiffers},
		{"a SKILL.md named otherwise", "skills/legion-worker/SKILL.md", frontmatter("legion-worker-2"), "", texts.NameDiffers},
		{"a SKILL.md named otherwise, with CRLF lines", "skills/asd-ste100/SKILL.md", "---\r\nname: asd\r\n---\r\nBody.\r\n", "", texts.NameDiffers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, filepath.FromSlash(tc.path)), tc.body)
			digest := tc.digest
			if digest == "" {
				digest = texts.Digest([]byte(legionText(tc.path)))
			}
			if err := texts.WriteManifest(dir, map[string]string{tc.path: digest}); err != nil {
				t.Fatal(err)
			}
			faults := texts.CheckPlugins(read(t, dir), filepath.Join(links, "envoy"), filepath.Join(links, "legion"))
			if tc.want == "" {
				if len(faults) != 0 {
					t.Errorf("CheckPlugins(%s) = %v, want no fault", tc.path, faults)
				}
				return
			}
			if len(faults) != 1 || faults[0].Path != tc.path || faults[0].Cause != tc.want || faults[0].Dir != dir {
				t.Fatalf("CheckPlugins(%s) = %+v, want one %s fault at it", tc.path, faults, tc.want)
			}
			if tc.want == texts.NameDiffers && !strings.Contains(faults[0].Detail, strings.TrimSuffix(filepath.Base(tc.path), ".md")) &&
				!strings.Contains(faults[0].Detail, filepath.Base(filepath.Dir(tc.path))) {
				t.Errorf("the name fault %q names no name its path gives", faults[0].Error())
			}
		})
	}
}

// The plugins' text directories a root is built from must each be readable: pi-envoy's skills,
// pi-legion's agents and pi-legion's skills. A plugin missing any of them is refused with the
// directory's own read error, and two plugins that hold all three are not.
func TestReadablePluginsReadsEachTextDirectory(t *testing.T) {
	if err := texts.ReadablePlugins(plugins(t)); err != nil {
		t.Fatalf("ReadablePlugins of two whole plugins = %v, want nil", err)
	}
	for _, gone := range []struct{ name, dir string }{
		{"pi-envoy's skills", "pi-envoy/dist/skills"},
		{"pi-legion's agents", "pi-legion/agents"},
		{"pi-legion's skills", "pi-legion/dist/skills"},
	} {
		envoy, legion := plugins(t)
		dir := filepath.Join(filepath.Dir(envoy), filepath.FromSlash(gone.dir))
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		err := texts.ReadablePlugins(envoy, legion)
		var path *fs.PathError
		if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &path) || path.Path != dir {
			t.Errorf("ReadablePlugins without %s = %v, want the read error of %s", gone.name, err, dir)
		}
	}
}

// Every agent and skill a folder text names, in each form a text can name one in, must be one the
// worker image ships: one row per form, a known name and an unknown one each, so a form the
// folder's reader misses lets its unknown name through and fails here. Each unknown name is one
// fault at the first file that names it; a boot that read no names makes a folder that names any
// one fault, NamesUnread, and a folder that names none nothing to check.
func TestEveryReferenceOfTheFolderMustShip(t *testing.T) {
	shipped := texts.NewShipped([]string{"oracle", "scout"}, []string{"dispatch", "legion-worker"})
	forms := []struct {
		name  string
		cause texts.Cause
		known string
		write func(name string) string
	}{
		{"agent equals a double-quoted name", texts.UnknownAgent, "oracle", func(n string) string { return `task(agent="` + n + `")` }},
		{"agent equals an escaped double-quoted name", texts.UnknownAgent, "oracle", func(n string) string { return `"task(agent=\"` + n + `\")"` }},
		{"agent equals a single-quoted name", texts.UnknownAgent, "scout", func(n string) string { return `task(agent='` + n + `')` }},
		{"agent colon a double-quoted name", texts.UnknownAgent, "oracle", func(n string) string { return `task({ agent: "` + n + `" })` }},
		{"a quoted agent key", texts.UnknownAgent, "scout", func(n string) string { return `{"agent": "` + n + `"}` }},
		{"a subagent_type under the plugin's namespace", texts.UnknownAgent, "oracle", func(n string) string { return `subagent_type: "legion:` + n + `"` }},
		{"a skill URL", texts.UnknownSkill, "dispatch", func(n string) string { return "Load skill://" + n + "." }},
		{"a skill URL with a path", texts.UnknownSkill, "legion-worker", func(n string) string { return "Read skill://" + n + "/references/x.md." }},
		{"a skill command", texts.UnknownSkill, "dispatch", func(n string) string { return "Run /skill:" + n + " now." }},
	}
	knownFile := func(i int) string { return fmt.Sprintf("roles/known-%d.md", i) }
	unknownFile := func(i int) string { return fmt.Sprintf("skills/legion-worker/references/unknown-%d.md", i) }
	unknownName := func(i int) string { return fmt.Sprintf("no-such-name-%d", i) }
	files := map[string][]byte{}
	for i, form := range forms {
		files[knownFile(i)] = []byte("Text before. " + form.write(form.known) + " Text after.\n")
		files[unknownFile(i)] = []byte("Text before. " + form.write(unknownName(i)) + " Text after.\n")
	}
	// One unknown name that two files name is one fault, at the first of them.
	files["agents/goal-red-team.md"] = []byte(`task(agent="twice-named")`)
	files["roles/reviewer.md"] = []byte(`task(agent="twice-named")`)
	folder := texts.Folder{Dir: "/folder", Files: files}

	faults := texts.CheckReferences(texts.References(folder), shipped)
	byPath := map[string]texts.Fault{}
	for _, f := range faults {
		if _, twice := byPath[f.Path]; twice {
			t.Errorf("two faults name %s: %v", f.Path, faults)
		}
		byPath[f.Path] = f
	}
	for i, form := range forms {
		if f, ok := byPath[unknownFile(i)]; !ok {
			t.Errorf("%s: the unknown name in %q was let through: the folder's reader does not read the form", form.name, form.write(unknownName(i)))
		} else if f.Cause != form.cause || !strings.Contains(f.Detail, unknownName(i)) {
			t.Errorf("%s: the unknown name is named as %+v, want %s naming %s", form.name, f, form.cause, unknownName(i))
		}
		if f, ok := byPath[knownFile(i)]; ok {
			t.Errorf("%s: the shipped name %s is named as %+v, want no fault", form.name, form.known, f)
		}
	}
	if f := byPath["agents/goal-red-team.md"]; f.Cause != texts.UnknownAgent || !strings.Contains(f.Detail, "twice-named") || !strings.Contains(f.Detail, "roles/reviewer.md") {
		t.Errorf("the agent two files name is named as %+v, want one fault at agents/goal-red-team.md naming it and both files", f)
	}
	if len(byPath) != len(forms)+1 {
		t.Errorf("CheckReferences named %d faults, want one for each of the %d unknown names: %v", len(byPath), len(forms)+1, faults)
	}

	if faults := texts.CheckReferences(texts.References(folder), texts.Shipped{}); len(faults) != 1 || faults[0].Cause != texts.NamesUnread {
		t.Errorf("CheckReferences with no names read = %v, want one %s fault", faults, texts.NamesUnread)
	}
	silent := texts.Folder{Files: map[string][]byte{"roles/reviewer.md": []byte("Review the change.\n")}}
	if faults := texts.CheckReferences(texts.References(silent), texts.Shipped{}); len(faults) != 0 {
		t.Errorf("CheckReferences of a folder that names nothing, with no names read = %v, want none: nothing is left unchecked", faults)
	}
	// A boot that read the names knows them even where it read none: every name the folder's texts
	// use is then one the image does not ship, never one that cannot be checked.
	names := texts.References(folder)
	none := texts.CheckReferences(names, texts.NewShipped(nil, nil))
	if want := len(names[promptrefs.TaskAgents]) + len(names[promptrefs.Skills]); len(none) != want {
		t.Errorf("CheckReferences with names read and none shipped = %v, want one fault for each of the %d names", none, want)
	}
	for _, f := range none {
		if f.Cause != texts.UnknownAgent && f.Cause != texts.UnknownSkill {
			t.Errorf("CheckReferences with names read and none shipped names %+v, want an unknown agent or skill", f)
		}
	}
}
