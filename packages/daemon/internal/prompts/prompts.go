// Package prompts composes the role instructions the daemon gives an OMP pane, from the role
// prompts it embeds, and says which launches read each of them (PartReaders).
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// texts are Legion's role-prompt texts, its two trees side by side at one root: roles/, the shared
// role prompts (each role's own part, the phase workers' cores and the mechanics fragments that
// Compose lists, and the controller's prompt, ControllerPromptPaths), and go/, the daemon's own
// parts.
//
//go:embed roles go/*.md
var texts embed.FS

// Texts is Legion's role-prompt texts as a folder of texts an experiment arm names mirrors them:
// "roles/<path>" is the shared prompt internal/prompts/roles/<path>, and "go/<path>" the daemon's
// part internal/prompts/go/<path>. Its root holds those two trees and nothing else.
func Texts() fs.FS { return texts }

// Parts is the ordered role-prompt files a launch concatenates before its addressing and
// deployment-instruction fragments (runtime.ComposePrompt). The whole text rides one file that one
// --append-system-prompt names, because OMP applies only the final occurrence of that flag.
type Parts struct {
	RolePromptPaths []string
}

// Composer keeps state-local copies of the shared role prompts and the daemon's additions.
type Composer struct {
	sharedDir string
	goDir     string
}

// eachPart calls visit with every file of an embedded prompt directory, by its path below that
// directory, in lexical order.
func eachPart(parts embed.FS, dir string, visit func(name string, body []byte) error) error {
	return fs.WalkDir(parts, dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := parts.ReadFile(path)
		if err != nil {
			return err
		}
		return visit(strings.TrimPrefix(path, dir+"/"), body)
	})
}

// RoleReferences are the task agents and skills the shared role prompts name, each file named
// `roles/<file>`: the daemon hands these prompts to every worker, so a gate or an image probe
// resolves what they name beside the plugin's own.
func RoleReferences() promptrefs.Names {
	names := promptrefs.New()
	if err := eachPart(texts, "roles", func(name string, body []byte) error {
		names.Text("roles/"+name, body)
		return nil
	}); err != nil {
		panic(fmt.Sprintf("prompts: read the embedded role prompts: %v", err))
	}
	return names
}

// New snapshots the embedded role prompts and the daemon's own parts below stateDir wherever the
// file there does not already hold them. The caller constructs it during daemon boot, and `legion
// controller start` for the controller's prompt. The files stay across restarts, since a resumed
// pane reads its prompt from the snapshot by path. The daemon owns these files; an operator's own
// text is the deployment `instructions`. A file already holding the current content is left as it
// is, so an ordinary restart changes nothing, and one an older daemon wrote is rewritten.
func New(stateDir string) (*Composer, error) {
	sharedDir := filepath.Join(stateDir, "prompts", "shared")
	if err := eachPart(texts, "roles", func(name string, body []byte) error {
		return syncPrompt(filepath.Join(sharedDir, name), body, "shared role prompt")
	}); err != nil {
		return nil, err
	}
	goDir := filepath.Join(stateDir, "prompts", "go")
	if err := eachPart(texts, "go", func(name string, body []byte) error {
		return syncPrompt(filepath.Join(goDir, name), body, "Go daemon prompt")
	}); err != nil {
		return nil, err
	}
	return &Composer{sharedDir: sharedDir, goDir: goDir}, nil
}

func syncPrompt(path string, body []byte, kind string) error {
	written, err := os.ReadFile(path)
	if err == nil && bytes.Equal(written, body) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s %s: %w", kind, path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s directory %s: %w", kind, filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s %s: %w", kind, path, err)
	}
	return nil
}

// ControllerPromptPaths are the snapshots of the controller's prompt parts, in order
// (controllerPartNames).
func (c *Composer) ControllerPromptPaths(headless bool) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("controller role prompt: nil composer")
	}
	return c.snapshots(controllerPartNames(headless)), nil
}

// Part is the daemon's prompt part name, as boot snapshotted it: what a backend's launch is told
// after its role's parts (runtime.BackendInfo.PromptPart).
func (c *Composer) Part(name string) string {
	return filepath.Join(c.goDir, name)
}

// PartNames are the parts Compose lists for role, in the order a launch reads them, each named as
// Texts names it: the shared role parts ("roles/<path>"), then this daemon's own ("go/<path>"):
// the role's own, then the text every architect (architect-common.md) or every phase worker
// (worker-common.md) shares. Compose's RolePromptPaths are their snapshots, one for each name in
// the same order, and PartReaders reads the same list.
func PartNames(role claim.Role, isRoot bool) ([]string, error) {
	switch role {
	case claim.RoleArchitect:
		name := "architect.md"
		if isRoot {
			name = "architect-root.md"
		}
		return []string{"roles/" + name, "go/" + name, "go/architect-common.md"}, nil
	case claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer:
		name := string(role) + ".md"
		return []string{
			"roles/core/common.md", "roles/core/" + name, "roles/mechanics/headless.md", "roles/" + name,
			"go/" + name, "go/worker-common.md",
		}, nil
	case claim.RoleMerger:
		return []string{"roles/mechanics/headless.md", "roles/merger.md", "go/merger.md", "go/worker-common.md"}, nil
	default:
		return nil, fmt.Errorf("compose role prompt: unsupported role %q", role)
	}
}

// controllerPartNames are the controller's prompt parts, named as Texts names them: its shared role
// part, then the daemon's part for how it was launched — headless in a pod the daemon supervises
// (`controller: daemon`), or in the operator's terminal by `legion controller start`.
func controllerPartNames(headless bool) []string {
	launch := "go/controller-interactive.md"
	if headless {
		launch = "go/controller-headless.md"
	}
	return []string{"roles/controller-root.md", launch}
}

// snapshot is the state-local copy New made of the part Texts names name.
func (c *Composer) snapshot(name string) string {
	if rest, ok := strings.CutPrefix(name, "roles/"); ok {
		return filepath.Join(c.sharedDir, rest)
	}
	return filepath.Join(c.goDir, strings.TrimPrefix(name, "go/"))
}

// snapshots are the snapshots of the parts names, in their order.
func (c *Composer) snapshots(names []string) []string {
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, c.snapshot(name))
	}
	return paths
}

// Compose returns the snapshots of the parts PartNames lists for role, in its order. The runtime
// appends addressing and deployment instructions after these paths.
func (c *Composer) Compose(role claim.Role, isRoot bool) (Parts, error) {
	if c == nil {
		return Parts{}, fmt.Errorf("compose role prompt: nil composer")
	}
	names, err := PartNames(role, isRoot)
	if err != nil {
		return Parts{}, err
	}
	return Parts{RolePromptPaths: c.snapshots(names)}, nil
}

// Readers are the launches whose prompt holds one part of Texts.
type Readers struct {
	// Roles are the issue roles whose launch composes the part, in either of a role's shapes (a
	// root and a child architect compose different parts).
	Roles []claim.Role
	// Controller is whether either of the controller's launches holds it (ControllerPromptPaths).
	Controller bool
	// Backends, when there are any, are the only harnesses whose launches of Roles are told the
	// part: it is what a launch on them is told after its role's parts (runtime.BackendInfo's
	// PromptPart). With none, a launch of Roles on any harness reads it.
	Backends []runtime.Backend
}

// PartReaders answers, for every file of Texts by its path there ("roles/<path>", "go/<path>"),
// the launches that read it: the roles whose parts Compose lists (PartNames), the controller's
// launches (ControllerPromptPaths), and the roles each harness runs that its launch tells its own
// part. A file no launch reads has an entry too, with no reader.
func PartReaders() map[string]Readers {
	readers := map[string]Readers{}
	if err := fs.WalkDir(texts, ".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			readers[path] = Readers{}
		}
		return err
	}); err != nil {
		panic(fmt.Sprintf("prompts: read the embedded role prompts: %v", err))
	}
	readBy := func(name string, role claim.Role) {
		r := readers[name]
		if !slices.Contains(r.Roles, role) {
			r.Roles = append(r.Roles, role)
		}
		readers[name] = r
	}
	for _, role := range claim.Roles {
		for _, isRoot := range []bool{true, false} {
			names, err := PartNames(role, isRoot)
			if err != nil {
				panic(fmt.Sprintf("prompts: %v", err))
			}
			for _, name := range names {
				readBy(name, role)
			}
		}
	}
	for _, headless := range []bool{true, false} {
		for _, name := range controllerPartNames(headless) {
			r := readers[name]
			r.Controller = true
			readers[name] = r
		}
	}
	for _, backend := range slices.Sorted(maps.Keys(runtime.Backends)) {
		info := runtime.Backends[backend]
		if info.PromptPart == "" {
			continue
		}
		name := "go/" + info.PromptPart
		for _, role := range claim.Roles {
			if info.Runs(role) {
				readBy(name, role)
			}
		}
		r := readers[name]
		r.Backends = append(r.Backends, backend)
		readers[name] = r
	}
	return readers
}
