package ghconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// Bundle is one role's gh directories: Default, the lowercased owner whose directory GH_CONFIG_DIR
// names (the one the issue's own repository routes to, RouteDir; the controller's asked-for or
// first owner), Dirs, each directory's Rendered files keyed by the directory's name under the
// bundle root, and Pairs, the configured renames that route a request by repository. A
// directory's name is a lowercased owner login (IsOwner) and nothing narrower,
// whose token is that account's installation lease and reaches every repository it holds. A
// workflow role's every directory carries that lease at the role's own permissions and none is
// read-only; a controller's bundle is the one made of reading leases (appauth's
// GitHubCredential and ControllerCredential, INFRA-2543). The name authorises nothing — the
// token in the directory does. A run from before INFRA-2543's change also wrote `<owner>/<repo>`
// directories under an owner's own; no bundle holds one now, and the one trace of that shape left
// here is WriteBundle's prune, which removes every subdirectory of an owner's directory such a
// run left behind.
//
// Pairs are the configured renames that moved a repository to another owner's installation and
// resolve for this project (ghrepo.Renames.Moving), which WriteBundle writes into the root as
// PairsFile. They are what makes the routing key the repository rather than the owner: a request
// for a pair's `from` repository is answered from the directory of the owner it moved to, whose
// installation holds it, while every other repository of the `from` owner is answered from that
// owner's own directory. The App being installed on both accounts of a pair is what makes the
// difference matter — one directory per owner cannot answer both — and it is this deployment's
// own case, where the implement App's sjawhar installation holds 151 repositories and not
// cybermonklabs/legion. A pair whose `to` owner the bundle holds no directory for is left in the
// file and refused by the wrapper that reads it (cmd/legion's ghConfigDirFor), naming the pair:
// no lease of this bundle reaches that repository, and answering the `from` owner's token
// instead would be a push that cannot land.
type Bundle struct {
	Default string
	Dirs    map[string]Rendered
	Pairs   []ghrepo.Rename
}

// ownerLogin is a lowercased GitHub login, ghrepo's ownerName lowercased: a letter or digit and
// then up to 38 letters, digits, hyphens and underscores. An underscore is admitted because a
// managed user's login ends in `_<shortcode>` (`mona-cat_octo`), and a repository of such an
// account is one ghrepo parses and the daemon admits an issue on: a rule here narrower than
// ghrepo's left that issue with no directory for its own repository, so no role of it launched.
// The 39 characters are GitHub's own bound, which no login exceeds. A login is a path segment
// under a bundle's root, so nothing else is one.
var ownerLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,38}$`)

// IsOwner says whether owner is a lowercased GitHub login, and so a directory a bundle may hold.
// It is the rule for every directory of a bundle, its Default among them, and for the owner a
// `gh` or a git-credential request names: a bundle's directory is one owner's whole lease, so
// there is no narrower name for one to have. PairsFile can never collide with it: its name holds
// a dot, which no GitHub login does.
func IsOwner(owner string) bool { return ownerLogin.MatchString(owner) }

// Dir is the directory an owner's two gh files live in under a bundle root: <root>/<owner>.
func Dir(root, name string) string { return filepath.Join(root, name) }

// RepositoryName is the one key a repository's every spelling has: lowercased, and without the
// `.git` a git credential request's path and a clone URL carry, which gh trims as it reads a
// remote (ghrepo.FromURL) and which no GitHub repository's own name ends in. One trim is all a
// spelling of such a name needs, so a second call changes nothing; a name ending `.git.git`
// would lose one suffix a call, and GitHub holds no repository of that name. The gh wrapper
// keys the repositories a command names by it, so two spellings of one repository are one
// repository (cmd/legion's gh_selectors).
func RepositoryName(repo string) string {
	return strings.TrimSuffix(strings.ToLower(repo), ".git")
}

// PairsFile is the bundle's own file in its root, beside the owners' directories: the configured
// renames a request is routed by (Bundle.Pairs), one pair a line, `<from> <to>`, each side an
// `<owner>/<name>` as GitHub spells it, in configured order and ending in a newline. A bundle
// with no pair to route by holds the file empty, so a root this daemon wrote always has it and a
// root without one was written before INFRA-2543's change (sandbox's movedGhDirs).
//
// It is a line a field rather than YAML because `legion git-credential` reads it on every object
// git asks a credential for, and a text line needs no decoder; a name GitHub allows holds no
// space and no newline (ghrepo.Parse), so the two fields of a line split exactly. Its name holds
// a dot, which no GitHub login does, so it can never collide with an owner's directory.
const PairsFile = "renames.txt"

// RenderPairs is pairs as PairsFile holds them, and "" for a bundle with none.
func RenderPairs(pairs []ghrepo.Rename) string {
	var text strings.Builder
	for _, pair := range pairs {
		text.WriteString(pair.From.String())
		text.WriteString(" ")
		text.WriteString(pair.To.String())
		text.WriteString("\n")
	}
	return text.String()
}

// ParsePairs reads what RenderPairs wrote: one pair a line, two `<owner>/<name>` names a line
// (ghrepo.Parse), blank lines ignored. A line that is no such pair is an error naming the line
// and its number, since a caller that routes by these pairs must fail rather than route by half
// of them; the text holds no token, so the error may quote it.
func ParsePairs(data []byte) ([]ghrepo.Rename, error) {
	var pairs []ghrepo.Rename
	for at, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		from, to, two := strings.Cut(line, " ")
		if !two {
			return nil, fmt.Errorf("line %d of the gh bundle's %s is no `<from> <to>` rename pair: %q", at+1, PairsFile, line)
		}
		moved, err := ghrepo.Parse("the repository a rename moved", from)
		if err != nil {
			return nil, fmt.Errorf("line %d of the gh bundle's %s: %w", at+1, PairsFile, err)
		}
		serving, err := ghrepo.Parse("the repository a rename moved it to", to)
		if err != nil {
			return nil, fmt.Errorf("line %d of the gh bundle's %s: %w", at+1, PairsFile, err)
		}
		pairs = append(pairs, ghrepo.Rename{From: moved, To: serving})
	}
	return pairs, nil
}

// ReadPairs is the pairs the bundle at root holds, read from PairsFile and from nowhere else
// (ParsePairs): the one way `legion gh` and `legion git-credential` learn what a configured
// rename moved, since the bundle the daemon writes is all either of them reads. A root with no
// such file holds no pair — a pod launched before this file existed projects none, and its drift
// check replaces it — while a file that cannot be read or parsed is an error, which the caller
// answers nothing on. It is read as ReadRegular reads, following the link a pod's Secret volume
// makes of a file at its top (readRegularFollowing), so a named pipe a pane put there stalls no
// call.
func ReadPairs(root string) ([]ghrepo.Rename, error) {
	data, err := readRegularFollowing(filepath.Join(root, PairsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParsePairs(data)
}

// RouteDir is the directory that answers for the repository owner/name in a bundle routing by
// pairs, lowercased: the `to` owner of the pair whose `from` is that repository, compared
// without case as GitHub compares a repository's two halves, and owner's own directory for
// every other repository. pair is the pair that routed it, and moved whether one did, so a
// caller refusing a directory the bundle lacks can name the pair that sent it there.
//
// It is the one rule a repository is routed to a directory by, so no two places can route one
// repository to two directories: `legion gh` and `legion git-credential` route every command
// and every credential request by it over the bundle's own pairs file (cmd/legion's
// ghConfigDirFor), and appauth's bundleDirs routes the issue's own repository by it to the
// bundle's Default, the directory GH_CONFIG_DIR names. A Default picked by the recorded owner
// alone is the `from` owner's directory for a repository a pair moved away from it, and where
// the App is installed on that owner too, that directory holds the owner's own lease, which
// cannot write the repository that moved.
func RouteDir(pairs []ghrepo.Rename, owner, name string) (dir string, pair ghrepo.Rename, moved bool) {
	repository := owner + "/" + name
	for _, pair := range pairs {
		if strings.EqualFold(pair.From.String(), repository) {
			return strings.ToLower(pair.To.Owner()), pair, true
		}
	}
	return strings.ToLower(owner), ghrepo.Rename{}, false
}

// RoutedByOwner is b as a reader that routes by owner alone must be handed it: a pod whose gh
// projection holds no PairsFile, launched before a bundle carried one, whose wrappers answer
// every repository of an owner from that owner's directory. Each pair's `from` owner's directory
// holds the `to` owner's lease, the one that reaches the repository the pair moved — and is left
// out where b holds no `to` directory, there being no lease of that repository to put there —
// and Default is recorded's own owner, the directory such a pod's GH_CONFIG_DIR was fixed at. That
// is the layout the pod was launched with, so it neither loses what it reaches nor gains what it
// cannot read until its update round replaces it with a pod that projects the pairs (sandbox's
// movedGhDirs). A zero recorded keeps b's Default: a controller's is the owner it asked for. b is
// not changed.
func (b Bundle) RoutedByOwner(recorded ghrepo.Repository) Bundle {
	routed := Bundle{Default: b.Default, Dirs: maps.Clone(b.Dirs), Pairs: b.Pairs}
	for _, pair := range b.Pairs {
		from := strings.ToLower(pair.From.Owner())
		if lease, held := b.Dirs[strings.ToLower(pair.To.Owner())]; held {
			routed.Dirs[from] = lease
		} else {
			delete(routed.Dirs, from)
		}
	}
	if !recorded.IsZero() {
		routed.Default = strings.ToLower(recorded.Owner())
	}
	return routed
}

// DirNames is the bundle's directory names, sorted.
func (b Bundle) DirNames() []string { return slices.Sorted(maps.Keys(b.Dirs)) }

// Validate refuses a bundle with no Default, a Default absent from Dirs or that is not a
// lowercased GitHub login, a directory that is not one either (IsOwner) — each name being a path
// under the bundle's root, and a `<owner>/<repo>` name among what it refuses — or a pair naming
// no repository on either side, which PairsFile could hold no line for.
//
// A pair whose `to` owner the bundle holds no directory for is no refusal here: the bundle is
// written and served, and the wrapper that routes a request to that missing directory is what
// refuses, naming the pair (cmd/legion's ghConfigDirFor). Refusing the bundle would leave the
// role no gh at all over a repository it may never touch.
func (b Bundle) Validate() error {
	if b.Default == "" {
		return errors.New("gh bundle names no default owner")
	}
	for _, name := range b.DirNames() {
		if !IsOwner(name) {
			return fmt.Errorf("gh bundle directory %q is no lowercased GitHub login", name)
		}
	}
	if !IsOwner(b.Default) {
		return fmt.Errorf("gh bundle default owner %q is not a lowercased GitHub login", b.Default)
	}
	if _, ok := b.Dirs[b.Default]; !ok {
		return fmt.Errorf("gh bundle default owner %q has no gh files in the bundle", b.Default)
	}
	for _, pair := range b.Pairs {
		if pair.From.IsZero() || pair.To.IsZero() {
			return fmt.Errorf("gh bundle rename pair %q -> %q names no repository on one side", pair.From, pair.To)
		}
	}
	return nil
}

// WriteBundle brings root to bundle: root made 0700, each owner's directory as Write brings one,
// the bundle's pairs written into the root as PairsFile, and both levels cleaned of what the
// bundle does not hold. An entry of the root that is neither an owner of the bundle nor that file
// goes — an owner no longer served, the two files of a directory written before bundles, which
// held one role's files at its top, and anything else — and so does every subdirectory of an
// owner's directory, the `<owner>/<repo>` directories a run of before INFRA-2543 cut a token per
// repository into, so none of them is left holding a stale token; the owner's own hosts.yml and
// config.yml stay. root is the bundle's alone, and it records nothing of the bundle but the gh
// files and those pairs: the pane's own user can write it, so a reader of the bundle's default or
// write owner reads the daemon's own record instead. It returns the directories whose hosts.yml
// changed, sorted, and writes nothing for a bundle Validate refuses.
//
// Because the pane's own user can write root, the cleaning follows no link and waits on no fifo
// (rolefile.RemoveTree): a link at the root, one at an owner's name among them, is unlinked and
// what it names is never opened, and under an owner's directory only subdirectories go, so a link
// there is left as it is. An entry the cleaning cannot remove, such as a link under a directory the
// pane made 0555, is left as it is and answered as an error naming it. root may sit under a state
// directory reached through a link, or be a link itself, so it is resolved once
// (filepath.EvalSymlinks) before the cleaning reads it, and RemoveTree, which refuses a parent
// that is a link, takes the directory that link names.
func WriteBundle(root string, bundle Bundle) (changed []string, err error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	// Sorted, so the directories a log line names come in one order.
	for _, name := range bundle.DirNames() {
		dirChanged, err := Write(Dir(root, name), bundle.Dirs[name])
		if err != nil {
			return nil, fmt.Errorf("write %s's gh files: %w", name, err)
		}
		if dirChanged {
			changed = append(changed, name)
		}
	}
	// The pairs file is the root's own, written whether or not the bundle has a pair to route
	// by: the two wrappers read what a configured rename moved from here and from nowhere else,
	// and an empty file says that nothing is routed, where a missing one says only that nothing
	// wrote it.
	if _, err := replaceFile(filepath.Join(root, PairsFile), RenderPairs(bundle.Pairs)); err != nil {
		return nil, fmt.Errorf("write the gh bundle's %s: %w", PairsFile, err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve the gh bundle's root %s: %w", root, err)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() == PairsFile && !entry.IsDir() {
			continue
		}
		if _, owned := bundle.Dirs[entry.Name()]; !owned || !entry.IsDir() {
			if err := rolefile.RemoveTree(filepath.Join(resolved, entry.Name())); err != nil {
				return nil, fmt.Errorf("remove %s from the gh bundle: %w", entry.Name(), err)
			}
			continue
		}
		// Nothing under an owner's directory but its own two files is the bundle's: a
		// subdirectory there was cut by a run of before INFRA-2543 for one repository, and goes.
		ownerDir := Dir(resolved, entry.Name())
		under, err := os.ReadDir(ownerDir)
		if err != nil {
			return nil, err
		}
		for _, sub := range under {
			if !sub.IsDir() {
				continue
			}
			if err := rolefile.RemoveTree(filepath.Join(ownerDir, sub.Name())); err != nil {
				return nil, fmt.Errorf("remove %s/%s from the gh bundle: %w", entry.Name(), sub.Name(), err)
			}
		}
	}
	return changed, nil
}

// EmptyBundle makes root an empty 0700 directory, removing whatever an earlier run wrote there:
// the gh directory of a controller whose gh acts as nobody (a project with no GitHub owner to
// mint for, or a daemon with no GitHub App), which GH_CONFIG_DIR still names so that no gh
// configuration elsewhere is read in its place, emptied so that no token an earlier run wrote
// outlives this one. The removal follows no link and waits on no fifo (rolefile.RemoveTree), as
// WriteBundle's cleaning does, since the controller's own user can write root; and root, which
// may sit under a state directory reached through a link or be a link itself, is resolved once
// (filepath.EvalSymlinks) first, so RemoveTree, which refuses a parent that is a link, takes the
// directory that link names. Each error names the path it failed on.
func EmptyBundle(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve the gh bundle's root %s: %w", root, err)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := rolefile.RemoveTree(filepath.Join(resolved, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
