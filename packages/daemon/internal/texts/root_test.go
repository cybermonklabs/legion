package texts_test

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// The root a launch of a folder loads first holds every pi-legion agent and every pi-envoy skill,
// and of pi-legion's skills only those whose files the folder holds, each a copy of the plugin's
// with the folder's files over it: so a skill whose one reference the folder changes keeps
// Legion's bytes for its other files, an agent the folder does not change is Legion's, and a
// pi-legion skill the folder does not touch is not copied, keeping the plugin's own rank. Every
// entry is a regular file or a directory, never a link, even where the plugin's own file is a
// link, and a later change to a plugin does not reach the root. The root holds no role part, and
// a folder file that replaces no plugin file, or a destination that already holds files, is
// refused.
func TestBuildRootLaysTheFolderOverLegionsSkill(t *testing.T) {
	envoy, legion := plugins(t)
	// An installed plugin's file can be a link; the root holds its bytes.
	linked := filepath.Join(t.TempDir(), "tools.md")
	writeFile(t, linked, legionText("skills/legion-worker/references/tools.md"))
	if err := os.Symlink(linked, filepath.Join(legion, "dist", "skills", "legion-worker", "references", "tools.md")); err != nil {
		t.Fatal(err)
	}
	changed := []string{"agents/goal-red-team.md", "skills/legion-worker/references/plan-tests.md", "skills/dispatch-first/SKILL.md"}
	dir := t.TempDir()
	digests := map[string]string{}
	for _, p := range append(slices.Clone(changed), "roles/reviewer.md") {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(p)), folderText(p))
		digests[p] = texts.Digest([]byte(legionText(p)))
	}
	if err := texts.WriteManifest(dir, digests); err != nil {
		t.Fatal(err)
	}
	folder := read(t, dir)

	dest := filepath.Join(t.TempDir(), "root")
	root, err := texts.BuildRoot(folder, envoy, legion, dest)
	if err != nil {
		t.Fatalf("BuildRoot = %v, want the root built", err)
	}
	if root.Dir != dest || !slices.Equal(root.Touched, []string{"dispatch-first", "legion-worker"}) {
		t.Errorf("BuildRoot = %+v, want the root at %s touching dispatch-first and legion-worker", root, dest)
	}

	want := map[string]string{
		// pi-legion's agents: the changed one the folder's, the others Legion's.
		"agents/goal-red-team.md": folderText("agents/goal-red-team.md"),
		"agents/oracle.md":        legionText("agents/oracle.md"),
		"agents/deep-worker.md":   legionText("agents/deep-worker.md"),
		// The touched pi-legion skill: its changed reference the folder's, the rest Legion's.
		"skills/legion-worker/SKILL.md":                 legionText("skills/legion-worker/SKILL.md"),
		"skills/legion-worker/references/plan-tests.md": folderText("skills/legion-worker/references/plan-tests.md"),
		"skills/legion-worker/references/tools.md":      legionText("skills/legion-worker/references/tools.md"),
		// Every pi-envoy skill, its licence too, and the folder's dispatch-first over it.
		"skills/dispatch-first/SKILL.md":   folderText("skills/dispatch-first/SKILL.md"),
		"skills/asd-ste100/SKILL.md":       legionText("skills/asd-ste100/SKILL.md"),
		"skills/asd-ste100/LICENSE":        legionText("skills/asd-ste100/LICENSE"),
		"skills/envoy/references/tools.md": legionText("skills/envoy/references/tools.md"),
	}
	got := map[string]string{}
	if err := filepath.WalkDir(dest, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dest, file)
		if !entry.Type().IsRegular() {
			t.Errorf("the root holds %s as %v, want a regular file: a copy, never a link", rel, entry.Type())
		}
		body, err := os.ReadFile(file)
		got[filepath.ToSlash(rel)] = string(body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		for _, p := range slices.Sorted(maps.Keys(want)) {
			if got[p] != want[p] {
				t.Errorf("the root holds %s as %q, want %q", p, got[p], want[p])
			}
		}
		for _, p := range slices.Sorted(maps.Keys(got)) {
			if _, ok := want[p]; !ok {
				t.Errorf("the root holds %s, which it should not: no role part, and no untouched pi-legion skill", p)
			}
		}
	}

	// The root holds the bytes it was built from: a plugin changed after the build, through the
	// file itself or the link's target, changes nothing in it.
	writeFile(t, filepath.Join(legion, "agents", "oracle.md"), "A later release.\n")
	writeFile(t, linked, "A later release.\n")
	for _, p := range []string{"agents/oracle.md", "skills/legion-worker/references/tools.md"} {
		if body, _ := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p))); string(body) != want[p] {
			t.Errorf("after the plugin changed, the root holds %s as %q, want the bytes it was built from", p, body)
		}
	}

	t.Run("a destination that holds files", func(t *testing.T) {
		if _, err := texts.BuildRoot(folder, envoy, legion, dest); err == nil || !strings.Contains(err.Error(), dest) {
			t.Errorf("BuildRoot into the built root = %v, want a refusal naming %s", err, dest)
		}
	})
	t.Run("a folder file that replaces no plugin file", func(t *testing.T) {
		extra := texts.Folder{Dir: dir, Files: maps.Clone(folder.Files), Manifest: folder.Manifest}
		extra.Files["agents/new-agent.md"] = []byte("A new agent.\n")
		if _, err := texts.BuildRoot(extra, envoy, legion, filepath.Join(t.TempDir(), "root")); err == nil || !strings.Contains(err.Error(), "agents/new-agent.md") {
			t.Errorf("BuildRoot of a folder adding an agent = %v, want a refusal naming agents/new-agent.md", err)
		}
	})
}
