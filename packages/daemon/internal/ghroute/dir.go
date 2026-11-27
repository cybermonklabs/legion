package ghroute

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// DirFor is the gh directory of the bundle under ownersDir that answers for the repository
// owner/name, the one pick every caller makes: `legion gh` routes every repository and owner a
// command names by it (TargetOf's dirFor), `legion git-credential` the repository a request's
// path names and the upstream that answers for a fork whose owner has no directory (ForkDir), and
// the daemon's gh tool every call of an answer over that answer's own bundle (internal/ghtool).
// It is the owner's own directory (ownerDir), except where a configured rename moved that one
// repository to another owner, which is the `to` owner's directory instead. name is the
// repository as the bundle keys one (ghconfig.RepositoryName) and is "" where the caller names an
// owner and no repository of it — a `gh` command's selector naming the owner alone, a credential
// request whose path carries one segment — which is the owner's own directory, the widest token
// the bundle holds for that account and never another's.
//
// The pairs are the bundle's own file, written by the daemon that wrote the directories
// (ghconfig.ReadPairs, ghconfig.PairsFile), and no other source is read. In a pod the file is a
// read-only projection of the role's Secret, which no agent can write. Under tmux the bundle's
// root is the pane user's own, so a pane can rewrite the file until the daemon's next refresh
// writes its own back (ghconfig.WriteBundle), and that hands out nothing new: a pair routes a
// request only to a directory of this bundle, and every one of those holds a lease of the
// role's own App, so a rewritten file reaches no token the pane does not already hold.
// The rule is ghconfig.RouteDir, which the daemon routes the bundle's own default by as well
// (appauth's bundleDirs), so the directory GH_CONFIG_DIR names for an issue is the one this
// answers for the issue's repository.
//
// A pair whose `to` owner the bundle holds no directory for is refused, naming the pair and the
// directory: no lease of this bundle reaches the repository that pair moved — the role's App is
// not installed on the owner holding it, or GitHub refused that lease — and the `from` owner's
// token, which is what an owner-only answer would hand out, cannot write it. Failing closed
// leaves git and gh saying so, where the other answer is a push that is refused at the remote.
// That refusal holds OwnerNotServedError's words inside a longer line, so the leak guard reads
// it as the setup failure it is and not as an owner the project does not serve. It names the
// repository by the pair alone, which holds it as configured, and never as the caller spelled it
// (Shown's rule): owner and name are what the call named.
func DirFor(ownersDir, owner, name string) (string, error) {
	if name == "" {
		return ownerDir(ownersDir, owner)
	}
	pairs, err := ghconfig.ReadPairs(ownersDir)
	if err != nil {
		return "", fmt.Errorf("read the gh bundle's configured renames: %w", err)
	}
	dir, pair, moved := ghconfig.RouteDir(pairs, owner, name)
	if !moved {
		return ownerDir(ownersDir, owner)
	}
	answer, err := ownerDir(ownersDir, dir)
	if err != nil {
		return "", fmt.Errorf("the configured rename %s -> %s serves that repository from %s's gh directory, and %w; GitHub holds the repository under the name it moved to, so no other token of this bundle reaches it",
			pair.From, pair.To, dir, err)
	}
	return answer, nil
}

// ownerDir is the gh directory of the bundle under ownersDir that answers for owner: `<owner>`,
// the one directory the bundle holds for that account, whose lease reaches every repository its
// installation holds (appauth.GitHubCredential). It is the answer for every repository of that
// owner but one a configured rename moved away from it, which DirFor routes by its name instead.
// ForkRepository calls it as well, to ask whether the bundle holds a directory for a request
// path's own owner at all, and ForkDir to ask it of a fork's parent's and source's owners.
//
// An owner the bundle holds no directory for is refused: the role's App has no installation on
// owner, so no lease of this bundle is that owner's own. The credential helper then reads whether
// the repository is a fork of one the bundle serves (ForkRepository, cmd/legion's
// forkUpstreamDir) and answers the token of the fork's parent's owner, else of its source's owner
// (ForkDir); every other repository of such an owner gets nothing. The refusal is
// leakguard.OwnerNotServedError, which the leak guard reads, exactly, in `legion gh`'s refusal of
// its REST visibility read: it then reads that repository's visibility with no credential
// instead.
func ownerDir(ownersDir, owner string) (string, error) {
	dir := ghconfig.Dir(ownersDir, owner)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, nil
	}
	return "", &leakguard.OwnerNotServedError{Owner: owner, OwnersDir: ownersDir}
}

// ForkRepository is the repository the credential helper reads from GitHub after DirFor refused a
// request (cmd/legion's forkUpstreamDir): owner, the path's owner already lowercased, and name,
// the path's second segment already keyed as the bundle keys a repository
// (ghconfig.RepositoryName: lowercased, without `.git`), the keyed name the caller routed with
// DirFor. Its caller asks only where that refusal is, or wraps, a leakguard.OwnerNotServedError: a
// pairs file that ghconfig.ReadPairs refuses makes no read, since neither the fork's parent nor
// its source could be routed by it either. There is one only where the bundle holds no directory
// for the path's own owner (ownerDir), an owner the role's App is not installed on, and where the
// two make an `<owner>/<name>` that ghrepo.Parse accepts. ok is false for anything else, and no
// read is made. A served owner whose repository a configured rename moved to an owner with no
// directory keeps DirFor's refusal, which is about that pair. A path with no second segment, or
// with one no GitHub repository could be named for, names no repository to read.
func ForkRepository(ownersDir, owner, name string) (repo ghrepo.Repository, ok bool) {
	var unserved *leakguard.OwnerNotServedError
	if _, err := ownerDir(ownersDir, owner); !errors.As(err, &unserved) {
		return ghrepo.Repository{}, false
	}
	repo, err := ghrepo.Parse("the repository the path names", owner+"/"+name)
	return repo, err == nil
}

// ForkDir is the gh directory that answers for the fork repo, a repository of an owner the bundle
// under ownersDir holds no directory for (ForkRepository), whose parent and source are the full
// names GitHub's answer for repo gives them, as GitHub spells them, and "" for one it gives none
// of. It is the one rule for the directory a fork's token comes from: `legion git-credential`
// answers git by it once GitHub said repo is a fork (cmd/legion's forkUpstreamDir), and so does
// `legion gh auth git-credential`, the helper of every git `legion gh` runs, which answers as
// `legion git-credential` does.
//
// It is R2's rule: where the bundle holds a directory for the owner of the fork's parent, the
// nearer of the two repositories it comes from, it is the answer a request for the parent would
// get; otherwise, where the bundle holds one for the owner of the fork's source, the answer a
// request for the source would get; and in every other case there is none. The owner's directory
// alone decides which of the two answers (upstreamOwnerServed): one GitHub does not name, or
// whose owner the bundle holds no directory for, is not routed at all, even where a configured
// rename would move it to an owner that has one. The one that answers is routed as a request for
// it would be (upstreamDir), so a configured rename that moved it applies, and where that routes
// nowhere, as a rename that moved it to an owner with no directory does, the fork gets nothing,
// as a request for it does, and a parent that routes nowhere leaves the source untried. route
// names repo, that directory and the repository that routed there, as GitHub spells it: `the
// repository the request names, a fork, routes to <dir> through its parent <full name>`, or `its
// source`. Where the parent routes nowhere, the error names repo and the parent, why the parent
// did not route, and that the source was not tried; otherwise it names repo, the parent and the
// source, and why each did not answer. repo is what the request named, so each names it by its
// place alone (Shown); the parent, the source and the directories are GitHub's answer and the
// bundle's.
func ForkDir(ownersDir string, repo ghrepo.Repository, parent, source string) (dir, route string, err error) {
	named := Shown(repo.String(), "the repository the request names")
	tried := make([]string, 0, 2)
	for _, upstream := range []struct {
		role     string
		fullName string
	}{{"parent", parent}, {"source", source}} {
		if upstream.fullName == "" {
			tried = append(tried, "GitHub names no "+upstream.role)
			continue
		}
		if err = upstreamOwnerServed(ownersDir, upstream.fullName); err != nil {
			tried = append(tried, fmt.Sprintf("the %s %s, since %v", upstream.role, upstream.fullName, err))
			continue
		}
		if dir, err = upstreamDir(ownersDir, upstream.fullName); err == nil {
			return dir, fmt.Sprintf("%s, a fork, routes to %s through its %s %s", named, dir, upstream.role, upstream.fullName), nil
		}
		if upstream.role == "parent" {
			return "", "", fmt.Errorf("GitHub answers that %s is a fork of %s, whose owner the bundle holds a directory for, and a request for %s answers nothing, so the fork gets nothing either and its source is not tried: %v",
				named, upstream.fullName, upstream.fullName, err)
		}
		tried = append(tried, fmt.Sprintf("the %s %s, since %v", upstream.role, upstream.fullName, err))
	}
	return "", "", fmt.Errorf("GitHub answers that %s is a fork, and neither repository it comes from routes to a directory of the bundle: %s",
		named, strings.Join(tried, "; "))
}

// upstreamDir is the gh directory DirFor routes the repository fullName to, as GitHub spells it:
// its owner lowercased, as a request's owner is, and its name keyed as the bundle keys one
// (ghconfig.RepositoryName). A fullName that ghrepo.Parse refuses routes nowhere, so no name in
// GitHub's answer can name a path outside the bundle.
func upstreamDir(ownersDir, fullName string) (string, error) {
	upstream, err := ghrepo.Parse("GitHub's full_name", fullName)
	if err != nil {
		return "", err
	}
	return DirFor(ownersDir, strings.ToLower(upstream.Owner()), ghconfig.RepositoryName(upstream.Name()))
}

// upstreamOwnerServed is nil where the bundle under ownersDir holds a directory for the owner of
// the repository fullName, as GitHub spells it, keyed as upstreamDir keys it (lowercased), and
// otherwise the error that says why not: ownerDir's refusal, in the words a request for a
// repository of that owner is refused with, or ghrepo.Parse's refusal of a fullName, which then
// names no owner.
func upstreamOwnerServed(ownersDir, fullName string) error {
	upstream, err := ghrepo.Parse("GitHub's full_name", fullName)
	if err != nil {
		return err
	}
	_, err = ownerDir(ownersDir, strings.ToLower(upstream.Owner()))
	return err
}
