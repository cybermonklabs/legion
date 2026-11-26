package texts

import (
	"errors"
	"strings"
)

// Cause is why a folder of texts is refused at load or boot, or why a launch of its arm falls back
// to the launch of no arm: the word a log line, a notice and a launcher's refusal carry.
type Cause string

const (
	// Gone is a folder that does not exist.
	Gone Cause = "gone"
	// Unreadable is a folder, or a file in it, that cannot be read, or a Legion text the check
	// compares a file with that cannot be read.
	Unreadable Cause = "unreadable"
	// NoManifest is a folder with no manifest.json.
	NoManifest Cause = "no-manifest"
	// ManifestUnreadable is a manifest that cannot be read, or is not of the manifest's form.
	ManifestUnreadable Cause = "manifest-unreadable"
	// Unlisted is a file of the folder that the manifest does not list.
	Unlisted Cause = "unlisted"
	// Missing is a file the manifest lists that the folder does not hold.
	Missing Cause = "missing"
	// BadPath is a path that is not clean and relative, or not under roles/, go/, agents/ or
	// skills/.
	BadPath Cause = "bad-path"
	// LinkEscapes is a link in the folder whose target is outside it.
	LinkEscapes Cause = "link-escapes"
	// DigestDiffers is a file whose recorded digest is not that of Legion's text at its path: the
	// folder was cut from another version of Legion's texts.
	DigestDiffers Cause = "digest"
	// NoLegionText is a file at a path where Legion has no text.
	NoLegionText Cause = "no-legion-text"
	// NameDiffers is a folder agent or SKILL.md whose frontmatter name is not the name its path
	// gives, so Legion's copy would still be offered beside it.
	NameDiffers Cause = "name-differs"
	// Unread is a role part that no role the experiment reaches reads.
	Unread Cause = "unread"
	// UnknownAgent is an agent a folder text names that the worker image does not ship.
	UnknownAgent Cause = "unknown-agent"
	// UnknownSkill is a skill a folder text names that the worker image does not ship.
	UnknownSkill Cause = "unknown-skill"
	// NamesUnread is a boot that read no list of the names the worker image ships, so the names
	// the folder's texts use cannot be checked.
	NamesUnread Cause = "names-unread"
	// TwoFolders is two experiments that each give a folder to one role; it is refused at load.
	TwoFolders Cause = "two-folders"
	// LauncherCannotCheck is a pod whose launcher predates folders of texts.
	LauncherCannotCheck Cause = "launcher-cannot-check"
	// InstallsPlugins is a launch that installs Legion's plugins after its start.
	InstallsPlugins Cause = "installs-plugins"
	// Changed is a folder that changed between the daemon's read and the launcher's.
	Changed Cause = "changed"
)

// Causes are every Cause, in the order of their declaration.
var Causes = []Cause{
	Gone, Unreadable, NoManifest, ManifestUnreadable, Unlisted, Missing, BadPath, LinkEscapes,
	DigestDiffers, NoLegionText, NameDiffers, Unread, UnknownAgent, UnknownSkill, NamesUnread,
	TwoFolders, LauncherCannotCheck, InstallsPlugins, Changed,
}

// says is what each cause says of the folder or of its file.
var says = map[Cause]string{
	Gone:                "the folder does not exist",
	Unreadable:          "cannot be read",
	NoManifest:          "the folder holds no " + ManifestName,
	ManifestUnreadable:  ManifestName + ` cannot be read as {"files": {"<path>": "sha256:<64 lower-case hex>"}}`,
	Unlisted:            "the folder holds this file, and " + ManifestName + " does not list it",
	Missing:             ManifestName + " lists this file, and the folder does not hold it",
	BadPath:             "is not a clean relative path under roles/, go/, agents/ or skills/",
	LinkEscapes:         "is a link to a path outside the folder",
	DigestDiffers:       "the digest " + ManifestName + " records is not that of Legion's text at this path",
	NoLegionText:        "Legion has no text at this path",
	NameDiffers:         "its frontmatter name is not the name its path gives, so Legion's copy would still be offered",
	Unread:              "no role the experiment reaches reads Legion's text at this path",
	UnknownAgent:        "names an agent the worker image does not ship",
	UnknownSkill:        "names a skill the worker image does not ship",
	NamesUnread:         "the daemon's boot read no list of the agents and skills the worker image ships, so the names the folder's texts use cannot be checked",
	TwoFolders:          "two experiments each give a folder of texts to one role, which would then read two folders",
	LauncherCannotCheck: "the pod's launcher predates folders of texts, so it cannot check them",
	InstallsPlugins:     "this launch installs Legion's plugins after its start, so their texts cannot be checked before it",
	Changed:             "the folder changed between the daemon's read and the launcher's",
}

// Fault is one cause found in a folder of texts: the folder, the file (by its path in the folder,
// or "" for the folder itself), the cause, and what the check saw, for the operator.
type Fault struct {
	// Dir is the folder; "" where the check that found the fault was not given it
	// (CheckReferences).
	Dir string
	// Path is the file, by its path in the folder; "" is the folder as a whole.
	Path   string
	Cause  Cause
	Detail string
}

// FaultOf is the *Fault err holds (errors.As), which is how Read refuses a folder, or, for an err
// that holds none, the folder unreadable, err saying why.
func FaultOf(err error) Fault {
	if fault := (*Fault)(nil); errors.As(err, &fault) {
		return *fault
	}
	return Fault{Cause: Unreadable, Detail: err.Error()}
}

// Error is the fault as one sentence for a log line and for the operator: the folder, the file,
// what is wrong, what the check saw, and the cause.
func (f Fault) Error() string {
	var b strings.Builder
	b.WriteString("texts folder")
	if f.Dir != "" {
		b.WriteString(" " + f.Dir)
	}
	b.WriteString(": ")
	if f.Path != "" {
		b.WriteString(f.Path + ": ")
	}
	if sentence, ok := says[f.Cause]; ok {
		b.WriteString(sentence)
	} else {
		b.WriteString("refused")
	}
	if f.Detail != "" {
		b.WriteString(" (" + f.Detail + ")")
	}
	b.WriteString(" [" + string(f.Cause) + "]")
	return b.String()
}
