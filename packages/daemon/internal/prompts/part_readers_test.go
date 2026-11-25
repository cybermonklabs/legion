package prompts

import (
	"bytes"
	"embed"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Every text of the two embedded trees is composed into the prompt of some launches and of no
// others, and an experiment arm that replaces a text must know which launches read
// it: a replacement no launch reads is refused at boot, and a launch reads the folder's text
// exactly where Legion's own is composed. PartReaders is that list, one entry for each file of
// Texts()'s two trees ("roles/<path>" and "go/<path>"). This test walks both trees and holds the
// list to what the code composes: a file PartReaders does not classify fails, so does a
// classification for a file that is not embedded, and so does one that Compose, ControllerPromptPaths
// or the part a backend's launch is told after its role's parts (runtime.Backends' PromptPart,
// what specs.launchBackend appends) disagrees with. The expected readers are built from those
// three, never from PartReaders, so a part added to Compose later fails here until the list says
// who reads it. The last subtest states today's answers by name, so the code and the list cannot
// agree on a wrong one together: two shared parts no launch reads, the controller's three, and the
// Claude Code part, which only Claude Code's roles read.
func TestEveryLegionPartHasItsReaders(t *testing.T) {
	texts := Texts()
	files := map[string]bool{}
	top, err := fs.ReadDir(texts, ".")
	if err != nil {
		t.Fatalf("read the root of Texts(): %v", err)
	}
	var dirs []string
	for _, entry := range top {
		dirs = append(dirs, entry.Name())
	}
	if want := []string{"go", "roles"}; !slices.Equal(dirs, want) {
		t.Fatalf("Texts() holds %q at its root, want %q (the two embedded trees, nothing else)", dirs, want)
	}
	if err := fs.WalkDir(texts, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		files[path] = true
		return nil
	}); err != nil {
		t.Fatalf("walk Texts(): %v", err)
	}

	// Texts() is the embedded trees themselves, byte for byte, and no other tree.
	embedded := map[string]bool{}
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		if err := eachPart(parts, dir, func(name string, body []byte) error {
			path := dir + "/" + name
			embedded[path] = true
			got, err := fs.ReadFile(texts, path)
			if err != nil {
				t.Errorf("Texts() cannot read %s, which is embedded: %v", path, err)
			} else if !bytes.Equal(got, body) {
				t.Errorf("Texts() holds %s with other bytes than the embedded part", path)
			}
			return nil
		}); err != nil {
			t.Fatalf("walk the embedded %s tree: %v", dir, err)
		}
	}
	if !maps.Equal(files, embedded) {
		t.Errorf("Texts() holds %q, want exactly the embedded files %q", slices.Sorted(maps.Keys(files)), slices.Sorted(maps.Keys(embedded)))
	}

	got := PartReaders()
	t.Run("every file has an entry and every entry a file", func(t *testing.T) {
		for _, path := range slices.Sorted(maps.Keys(files)) {
			if _, ok := got[path]; !ok {
				t.Errorf("PartReaders() does not classify %s: say which roles, backends or the controller read it (none is an answer too)", path)
			}
		}
		for _, path := range slices.Sorted(maps.Keys(got)) {
			if !files[path] {
				t.Errorf("PartReaders() classifies %s, which no embedded tree holds", path)
			}
		}
	})

	want := composedReaders(t)
	t.Run("each entry is what Compose, ControllerPromptPaths and the backend's part compose", func(t *testing.T) {
		for _, path := range slices.Sorted(maps.Keys(files)) {
			have, ok := got[path]
			if !ok {
				continue
			}
			if !sameReaders(normalReaders(have), want[path]) {
				t.Errorf("PartReaders()[%q] = %s, but the launches compose it for %s", path, describeReaders(normalReaders(have)), describeReaders(want[path]))
			}
		}
	})

	t.Run("today's answers", func(t *testing.T) {
		claudeRoles := runtime.Backends[runtime.BackendClaude].Roles
		workers := []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger}
		for _, tc := range []struct {
			path string
			want Readers
		}{
			{"roles/core/oracle.md", Readers{}},
			{"roles/mechanics/interactive.md", Readers{}},
			{"roles/controller-root.md", Readers{Controller: true}},
			{"go/controller-headless.md", Readers{Controller: true}},
			{"go/controller-interactive.md", Readers{Controller: true}},
			{"go/claude-mechanics.md", Readers{Roles: claudeRoles, Backends: []runtime.Backend{runtime.BackendClaude}}},
			{"roles/core/common.md", Readers{Roles: []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer}}},
			{"roles/mechanics/headless.md", Readers{Roles: workers}},
			{"roles/architect.md", Readers{Roles: []claim.Role{claim.RoleArchitect}}},
			{"roles/architect-root.md", Readers{Roles: []claim.Role{claim.RoleArchitect}}},
			{"go/architect-common.md", Readers{Roles: []claim.Role{claim.RoleArchitect}}},
			{"go/worker-common.md", Readers{Roles: workers}},
			{"roles/merger.md", Readers{Roles: []claim.Role{claim.RoleMerger}}},
			{"go/merger.md", Readers{Roles: []claim.Role{claim.RoleMerger}}},
		} {
			have, ok := got[tc.path]
			if !ok {
				t.Errorf("PartReaders() does not classify %s", tc.path)
				continue
			}
			if !sameReaders(normalReaders(have), normalReaders(tc.want)) {
				t.Errorf("PartReaders()[%q] = %s, want %s", tc.path, describeReaders(normalReaders(have)), describeReaders(normalReaders(tc.want)))
			}
		}
	})
}

// composedReaders is the readers of every file of both trees as the code composes them, never as
// PartReaders says: Compose for each issue role in both of its shapes (a root and a child
// architect read different parts), ControllerPromptPaths for both of the controller's launches,
// and the part each backend's launch is told after its role's parts, read by the roles that
// backend runs. A file none of them lists has no reader.
func composedReaders(t *testing.T) map[string]Readers {
	t.Helper()
	state := t.TempDir()
	composer, err := New(state)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shared, own := filepath.Join(state, "prompts", "shared"), filepath.Join(state, "prompts", "go")
	// key is the path a snapshotted part has in Texts(): the shared tree is "roles", the daemon's own
	// parts "go".
	key := func(path string) string {
		for dir, root := range map[string]string{"roles": shared, "go": own} {
			if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
				return dir + "/" + filepath.ToSlash(rel)
			}
		}
		t.Fatalf("a composed part %s is under neither snapshot directory (%s, %s)", path, shared, own)
		return ""
	}
	readers := map[string]Readers{}
	// Every file starts with no reader, so a file nothing composes is an entry the list must hold.
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		if err := eachPart(parts, dir, func(name string, _ []byte) error {
			readers[dir+"/"+name] = Readers{}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(path string, with func(*Readers)) {
		r, ok := readers[path]
		if !ok {
			t.Fatalf("the code composes %s, which is no file of either embedded tree", path)
		}
		with(&r)
		readers[path] = r
	}
	for _, role := range claim.Roles {
		for _, isRoot := range []bool{true, false} {
			if role != claim.RoleArchitect && isRoot {
				continue
			}
			parts, err := composer.Compose(role, isRoot)
			if err != nil {
				t.Fatalf("Compose(%s, %t): %v", role, isRoot, err)
			}
			for _, path := range parts.RolePromptPaths {
				add(key(path), func(r *Readers) {
					if !slices.Contains(r.Roles, role) {
						r.Roles = append(r.Roles, role)
					}
				})
			}
		}
	}
	for _, headless := range []bool{true, false} {
		paths, err := composer.ControllerPromptPaths(headless)
		if err != nil {
			t.Fatalf("ControllerPromptPaths(%t): %v", headless, err)
		}
		for _, path := range paths {
			add(key(path), func(r *Readers) { r.Controller = true })
		}
	}
	for backend, info := range runtime.Backends {
		if info.PromptPart == "" {
			continue
		}
		path := key(composer.Part(info.PromptPart))
		for _, role := range claim.Roles {
			if info.Runs(role) {
				add(path, func(r *Readers) { r.Roles = append(r.Roles, role) })
			}
		}
		add(path, func(r *Readers) { r.Backends = append(r.Backends, backend) })
	}
	for path, r := range readers {
		readers[path] = normalReaders(r)
	}
	return readers
}

// normalReaders is r with its roles in claim.Roles' order, once each, and its backends sorted, so
// two answers that name the same launches compare equal.
func normalReaders(r Readers) Readers {
	var roles []claim.Role
	for _, role := range claim.Roles {
		if slices.Contains(r.Roles, role) {
			roles = append(roles, role)
		}
	}
	backends := slices.Clone(r.Backends)
	slices.Sort(backends)
	return Readers{Roles: roles, Controller: r.Controller, Backends: slices.Compact(backends)}
}

func sameReaders(a, b Readers) bool {
	return slices.Equal(a.Roles, b.Roles) && a.Controller == b.Controller && slices.Equal(a.Backends, b.Backends)
}

func describeReaders(r Readers) string {
	var who []string
	for _, role := range r.Roles {
		who = append(who, string(role))
	}
	if r.Controller {
		who = append(who, "the controller")
	}
	if len(who) == 0 {
		who = []string{"no launch"}
	}
	text := strings.Join(who, ", ")
	if len(r.Backends) > 0 {
		var backends []string
		for _, backend := range r.Backends {
			backends = append(backends, string(backend))
		}
		text += " on " + strings.Join(backends, ", ")
	}
	return text
}
