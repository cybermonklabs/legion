package texts

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// CheckRoleParts holds each role part of f (its files under roles/ and go/) to Legion's own, legion
// being the daemon's embedded parts (prompts.Texts): a path where Legion has no text, a digest the
// manifest records that is not that of Legion's text at the path, and a part read reports that no
// role the experiment reaches reads. It names one fault for each such file, in the order of their
// paths.
func CheckRoleParts(f Folder, legion fs.FS, read func(path string) bool) []Fault {
	var faults []Fault
	for _, p := range f.paths("roles/", "go/") {
		cause, detail := rolePartFault(f, legion, read, p)
		if cause != "" {
			faults = append(faults, Fault{Dir: f.Dir, Path: p, Cause: cause, Detail: detail})
		}
	}
	return faults
}

// rolePartFault is CheckRoleParts' answer for the role part at p: its cause and detail, or no
// cause.
func rolePartFault(f Folder, legion fs.FS, read func(path string) bool, p string) (Cause, string) {
	info, err := fs.Stat(legion, p)
	if errors.Is(err, fs.ErrNotExist) || err == nil && info.IsDir() {
		return NoLegionText, ""
	}
	var body []byte
	if err == nil {
		body, err = fs.ReadFile(legion, p)
	}
	switch {
	case err != nil:
		return Unreadable, "Legion's text: " + err.Error()
	case Digest(body) != f.Manifest.Files[p]:
		return DigestDiffers, digestDetail(f.Manifest.Files[p], Digest(body))
	case !read(p):
		return Unread, ""
	}
	return "", ""
}

func digestDetail(recorded, legion string) string {
	return fmt.Sprintf("%s records %s, and Legion's text is %s", ManifestName, recorded, legion)
}

// CheckPlugins holds each agent and skill file of f to the plugin file the launch loads: envoyRoot
// and legionRoot are the roots of the pi-envoy and pi-legion plugins as the launch loads them.
// "agents/<p>" is pi-legion's agents/<p>, and "skills/<skill>/<p>" is dist/skills/<skill>/<p> of
// whichever plugin ships that skill. A skill's LICENSE file and a path neither plugin ships have no
// Legion text. Each file must also record the digest of the plugin's file, and a folder agent's
// or SKILL.md's frontmatter name must be the one its path gives (an agent's file name, a skill's
// directory): Oh My Pi names an agent by its frontmatter alone and loads none without a name, and a
// skill whose name differs would sit beside Legion's copy rather than over it. It names one fault
// for each such file, in the order of their paths.
func CheckPlugins(f Folder, envoyRoot, legionRoot string) []Fault {
	var faults []Fault
	for _, p := range f.paths("agents/", "skills/") {
		cause, detail := pluginFault(f, envoyRoot, legionRoot, p)
		if cause != "" {
			faults = append(faults, Fault{Dir: f.Dir, Path: p, Cause: cause, Detail: detail})
		}
	}
	return faults
}

// pluginFault is CheckPlugins' answer for the agent or skill file at p: its cause and detail, or no
// cause.
func pluginFault(f Folder, envoyRoot, legionRoot, p string) (Cause, string) {
	shipped, ok := pluginText(p, envoyRoot, legionRoot)
	if !ok {
		return NoLegionText, ""
	}
	info, err := os.Stat(shipped)
	if errors.Is(err, fs.ErrNotExist) || err == nil && info.IsDir() {
		return NoLegionText, ""
	}
	var body []byte
	if err == nil {
		body, err = readRegular(shipped)
	}
	if err != nil {
		return Unreadable, "Legion's text: " + err.Error()
	}
	if Digest(body) != f.Manifest.Files[p] {
		return DigestDiffers, digestDetail(f.Manifest.Files[p], Digest(body)) + " at " + shipped
	}
	if detail, differs := nameDiffers(p, f.Files[p]); differs {
		return NameDiffers, detail
	}
	return "", ""
}

// pluginText is the plugin file the folder's file at p replaces, and false where no plugin ships
// one: pi-legion's agents/<p>, or dist/skills/<skill>/<p> of the plugin that ships the skill.
func pluginText(p, envoyRoot, legionRoot string) (string, bool) {
	if rest, ok := strings.CutPrefix(p, "agents/"); ok {
		return filepath.Join(legionRoot, "agents", filepath.FromSlash(rest)), true
	}
	skill, rest, ok := strings.Cut(strings.TrimPrefix(p, "skills/"), "/")
	if !ok || isLicense(path.Base(rest)) {
		return "", false
	}
	plugin, ok := skillPlugin(skill, envoyRoot, legionRoot)
	if !ok {
		return "", false
	}
	return filepath.Join(skillsDir(plugin), skill, filepath.FromSlash(rest)), true
}

// skillsDir is a plugin's skills directory, as its manifest names it (`omp.skills`).
func skillsDir(plugin string) string { return filepath.Join(plugin, "dist", "skills") }

// skillPlugin is the root of the plugin that ships skill: pi-envoy's, or else pi-legion's.
func skillPlugin(skill, envoyRoot, legionRoot string) (string, bool) {
	for _, plugin := range []string{envoyRoot, legionRoot} {
		if info, err := os.Stat(filepath.Join(skillsDir(plugin), skill)); err == nil && info.IsDir() {
			return plugin, true
		}
	}
	return "", false
}

// isLicense is whether a file name is a licence, which the pack ships beside a skill's texts and
// which is no text of Legion's.
func isLicense(name string) bool {
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "LICENSE") || strings.HasPrefix(upper, "LICENCE")
}

// nameDiffers says how the frontmatter name of the folder's agent or SKILL.md at p differs from
// the name its path gives, and whether it does: an agent at agents/<name>.md must be named <name>,
// and a SKILL.md under skills/<skill>/ that names itself must name <skill>. Any other file has no
// name to differ.
func nameDiffers(p string, body []byte) (string, bool) {
	name, named := frontmatterName(body)
	if rest, ok := strings.CutPrefix(p, "agents/"); ok {
		want := strings.TrimSuffix(rest, ".md")
		if !named {
			return fmt.Sprintf("its frontmatter gives no name, and its path names %s", want), true
		}
		return fmt.Sprintf("its frontmatter names %q, and its path %s", name, want), name != want
	}
	skill, rest, _ := strings.Cut(strings.TrimPrefix(p, "skills/"), "/")
	if rest != "SKILL.md" || !named {
		return "", false
	}
	return fmt.Sprintf("its frontmatter names %q, and its path %s", name, skill), name != skill
}

// frontmatterName is the `name` of body's YAML frontmatter, the block between a first line `---`
// and the next line `---`, and whether it gives one as a string.
func frontmatterName(body []byte) (string, bool) {
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", false
	}
	// The newline before rest lets an empty block (`---` twice) end at its first line.
	block, _, ok := strings.Cut("\n"+rest, "\n---\n")
	if !ok {
		if block, ok = strings.CutSuffix("\n"+rest, "\n---"); !ok {
			return "", false
		}
	}
	var front map[string]any
	if yaml.Unmarshal([]byte(block), &front) != nil {
		return "", false
	}
	name, ok := front["name"].(string)
	return name, ok
}

// References are the agents and skills f's files name (promptrefs), each with the files that name
// it, by their paths in the folder.
func References(f Folder) promptrefs.Names {
	names := promptrefs.New()
	for _, p := range f.paths(prefixes...) {
		names.Text(p, f.Files[p])
	}
	return names
}

// Shipped are the agents and skills the worker image ships, as the daemon's boot read them. The
// zero Shipped is a boot that read none, for which every name is unknown.
type Shipped struct {
	known          bool
	agents, skills map[string]bool
}

// ShippedNames are the names a boot read: every agent and every skill the image ships.
func ShippedNames(agents, skills []string) Shipped {
	s := Shipped{known: true, agents: map[string]bool{}, skills: map[string]bool{}}
	for _, agent := range agents {
		s.agents[agent] = true
	}
	for _, skill := range skills {
		s.skills[skill] = true
	}
	return s
}

// Known is whether the boot read the names.
func (s Shipped) Known() bool { return s.known }

// CheckReferences holds every agent and skill names holds to those the image ships: one fault for
// each name it does not ship, at the first file that names it, in the order of the names. Where the
// boot read no names, a folder whose texts name any agent or skill is one NamesUnread fault, since
// none of its names can be checked; one that names none has nothing to check. A fault names no
// folder, since names does not say which it came from.
func CheckReferences(names promptrefs.Names, shipped Shipped) []Fault {
	if !shipped.known {
		if len(names[promptrefs.TaskAgents]) > 0 || len(names[promptrefs.Skills]) > 0 {
			return []Fault{{Cause: NamesUnread}}
		}
		return nil
	}
	var faults []Fault
	for _, kind := range []struct {
		kind    promptrefs.Kind
		ships   map[string]bool
		cause   Cause
		article string
	}{
		{promptrefs.TaskAgents, shipped.agents, UnknownAgent, "agent"},
		{promptrefs.Skills, shipped.skills, UnknownSkill, "skill"},
	} {
		for _, name := range slices.Sorted(maps.Keys(names[kind.kind])) {
			if kind.ships[name] {
				continue
			}
			files := slices.Sorted(slices.Values(names[kind.kind][name]))
			faults = append(faults, Fault{Path: files[0], Cause: kind.cause,
				Detail: fmt.Sprintf("the %s %s, named by %s", kind.article, name, strings.Join(files, ", "))})
		}
	}
	return faults
}
