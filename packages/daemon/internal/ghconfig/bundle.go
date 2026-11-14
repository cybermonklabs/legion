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
)

// Bundle is one role's gh directories: Default, the lowercased owner whose directory GH_CONFIG_DIR
// names (the issue's owner; the controller's first owner), Dirs, each directory's Rendered
// files keyed by the directory's name under the bundle root, and Pairs, the configured renames
// that route a request by repository. A directory's name is a lowercased owner login,
// whose token is that account's installation lease and reaches every repository it holds. A
// workflow role's every directory carries that lease at the role's own permissions and none is
// read-only; a controller's bundle is the one made of reading leases (appauth's
// GitHubCredential and ControllerCredential, INFRA-2543). The name authorises nothing — the
// token in the directory does. No bundle this daemon builds holds a `<owner>/<repo>` directory
// (appauth's bundleDirs), and this package answers for that shape (IsDirName) only to clean up
// after a run of before that ruling: WriteBundle prunes the repository directories such a run
// left under an owner's own, and the Sandbox drift check reads them off a pod launched then
// (sandbox's movedGhDirs).
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

// ownerLogin is a lowercased GitHub login: letters, digits and hyphens, never starting with a
// hyphen. A login is a path segment under a bundle's root, so nothing else is one.
var ownerLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// repositoryName is a lowercased GitHub repository name: at most 100 letters, digits, `-`, `_`
// and `.`, which is GitHub's own grammar (ghrepo.Parse) lowercased. A name is a path segment
// under an owner's directory, and `.` and `..` are refused beside it, since either would name
// another directory than the repository's.
var repositoryName = regexp.MustCompile(`^[a-z0-9._-]{1,100}$`)

// IsOwner says whether owner is a lowercased GitHub login, and so a directory a bundle may hold.
// It is the rule for a caller that means an owner and nothing narrower: the bundle's Default,
// which GH_CONFIG_DIR names, and the owner a `gh` or a git-credential request names.
func IsOwner(owner string) bool { return ownerLogin.MatchString(owner) }

// IsDirName says whether name is a directory a bundle may hold: a lowercased owner login
// (IsOwner), or that login and one of its repositories as `<owner>/<repo>`, both lowercased. A
// GitHub login holds no slash and a repository name holds none either, so the one slash of a
// name splits it exactly.
//
// HostsFile and ConfigFile are no repository directory, though both are repository names GitHub
// allows: an owner's own two gh files sit beside a repository directory, so a bundle holding
// `<owner>/hosts.yml` would have WriteBundle make a directory where that owner's hosts.yml is
// and fail every directory of the bundle. PairsFile needs no such rule: it is the root's own
// file, and its name holds a dot, which no GitHub login does, so no directory of a bundle is
// ever named for it.
//
// No bundle this daemon builds holds a repository directory at all: a role's token reaches every
// repository its App's installation holds, so one directory per owner carries it (appauth's
// bundleDirs). The `<owner>/<repo>` shape stays a name this answers for, because a root an
// earlier run wrote still holds such directories, which WriteBundle prunes, and a pod launched
// before that change projects them, which the Sandbox drift check reports (sandbox's
// movedGhDirs). No token is read out of one: `legion gh` and `legion git-credential` answer for
// every repository of an owner from that owner's own directory.
func IsDirName(name string) bool {
	owner, repo, nested := strings.Cut(name, "/")
	if !IsOwner(owner) {
		return false
	}
	if !nested {
		return true
	}
	return repo != "." && repo != ".." && repo != HostsFile && repo != ConfigFile && repositoryName.MatchString(repo)
}

// Dir is the directory a bundle entry's two gh files live in under a bundle root: <root>/<name>,
// which for a `<owner>/<repo>` entry is a directory under the owner's own.
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
// root without one was written before that ruling (sandbox's movedGhDirs).
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
// answers nothing on.
func ReadPairs(root string) ([]ghrepo.Rename, error) {
	data, err := os.ReadFile(filepath.Join(root, PairsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParsePairs(data)
}

// DirNames is the bundle's directory names, sorted. An owner sorts before its own repositories'
// directories, since each of those begins with the owner's name.
func (b Bundle) DirNames() []string { return slices.Sorted(maps.Keys(b.Dirs)) }

// Validate refuses a bundle with no Default, a Default absent from Dirs or that is not a
// lowercased GitHub login — GH_CONFIG_DIR always names an owner's directory, never a
// repository's — a directory that is neither shape a bundle holds (IsDirName), each name being a
// path under the bundle's root, or a pair naming no repository on either side, which PairsFile
// could hold no line for.
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
		if !IsDirName(name) {
			return fmt.Errorf("gh bundle directory %q is no lowercased GitHub login or owner/repository", name)
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

// WriteBundle brings root to bundle: root made 0700, each directory as Write brings one, the
// bundle's pairs written into the root as PairsFile, and both levels cleaned of what the bundle
// no longer holds. An entry of the root that is neither an owner of the
// bundle nor that file goes — an owner no longer served, the two files of a directory written
// before bundles, which held one role's files at its top, and anything else — and under each
// owner's directory a
// subdirectory that is no repository entry of the bundle goes too, so a repository the mapping no
// longer serves is left holding no stale token; the owner's own hosts.yml and config.yml stay.
// root is the bundle's alone, and it records nothing of the bundle but the gh files and those
// pairs: the pane's own
// user can write it, so a reader of the bundle's default or write owner reads the daemon's own
// record instead. It returns the directories whose hosts.yml changed, sorted, and writes nothing
// for a bundle Validate refuses.
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
	// Sorted, so an owner's own directory is written before any of its repositories' are made
	// under it.
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
	// held is each owner of the bundle by the repository directories it holds, which is the whole
	// of what either level of the root may keep.
	held := map[string]map[string]bool{}
	for name := range bundle.Dirs {
		owner, repo, nested := strings.Cut(name, "/")
		repos, ok := held[owner]
		if !ok {
			repos = map[string]bool{}
			held[owner] = repos
		}
		if nested {
			repos[repo] = true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() == PairsFile && !entry.IsDir() {
			continue
		}
		repos, owned := held[entry.Name()]
		if !owned || !entry.IsDir() {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return nil, fmt.Errorf("remove %s from the gh bundle: %w", entry.Name(), err)
			}
			continue
		}
		ownerDir := Dir(root, entry.Name())
		under, err := os.ReadDir(ownerDir)
		if err != nil {
			return nil, err
		}
		for _, sub := range under {
			if !sub.IsDir() || repos[sub.Name()] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(ownerDir, sub.Name())); err != nil {
				return nil, fmt.Errorf("remove %s/%s from the gh bundle: %w", entry.Name(), sub.Name(), err)
			}
		}
	}
	return changed, nil
}
