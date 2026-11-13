package sandbox

import (
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The cache trimmer's two lists are written out in internal/launcher/volumepaths.go, because the
// launcher's own process is told none of the pod's paths: a roles container's environment is the
// pod's uid and nothing else, and TMPDIR, GOCACHE, XDG_CACHE_HOME and BUN_INSTALL_CACHE_DIR reach
// a role only in the start environment the daemon renders for each generation. So the lists are a
// copy of what this package renders, and a copy is what goes stale: the cache a later change adds
// to roleEnvironment lands on the volume named by neither list, where the trimmer either deletes
// it with nothing red or leaves it and the volume fills anyway. This test is what makes that edit
// fail here rather than in a pod.
//
// The rule is the one ProtectedPaths' own comment states, and it is deliberately narrower than
// "the lists mention it": the reclaimable list names a path only as that path itself, and the
// protected list names it as that path itself or as an ancestor of it. Coverage by a reclaimable
// root never counts, because $TMPDIR and the cache home are roots of which only part is eligible
// — one `go-build*` directory, one immediate child that is neither of the two exceptions. A new
// directory under either would otherwise be accounted for by a root that deletes none of it (a
// role's work under $TMPDIR, which the trimmer may not touch and which therefore still fills the
// volume) or by one that deletes all of it (a child of the cache home a role cannot afford to
// lose, as the browser registry and the Claude npm cache cannot be). The same narrowness is why
// reclaim_doc_test.go reads docs/kubernetes.md's two lists as lists rather than as prose: a check
// that accepts a mention anywhere cannot fail where it matters.
//
// A path both lists name is a contradiction and fails too: the trimmer would delete entries of a
// root it is told at the same line never to write to, and which of the two the code follows is
// then a reading of the code rather than of the lists.
const (
	// renderedIssueVolumePaths is the floor the issue's check names for the set this test walks:
	// the ten values the start environment writes under the volume — roleEnvironment's eight less
	// XDG_CONFIG_HOME, which is under the pod's in-memory home and off the volume, plus the issue
	// pod's two uv paths and PUBLIC_LEAK_CHECK, with LEGION_WORKSPACE dropped and the six gh
	// variables pointing at /var/run/legion/gh — and the three mounts and constants beside them
	// (issueVolumePaths). The set itself is derived from the renderer and never written out, so
	// this number is only a floor: it exists because a derivation that broke and matched nothing
	// would otherwise hold the two lists to an empty set and report agreement.
	renderedIssueVolumePaths = 13
	// newToolCache, newMount and newAgentCache are the three additions the negative controls make
	// to a copy of what the renderer gave. Each is a path no list names, and each stands for the
	// edit a later change really makes: a cache under the cache home, a mount of the volume, and a
	// Legion install block of its own beside Claude Code's.
	newToolCache  = "legion-a-new-tools-cache"
	newMount      = TreeRoot + "/artifacts"
	newAgentCache = "legion-a-new-agent"
)

// Every path the pod renderer puts on the issue's volume is named by one of the trimmer's two
// lists, and every cache the pod's own install blocks render under the cache home is on the
// protected list. Both halves read the renderer rather than a list written beside it: a list here
// would have to be edited by whoever adds a cache, which is the edit that gets forgotten.
func TestEveryPathTheRendererPutsOnTheIssueVolumeIsNamedByTheTrimmersLists(t *testing.T) {
	const issue = "LEGION-1234"
	const role = claim.RoleImplementer

	protected := protectedList()
	environment, mounts := issueVolumePaths(t, issue, role)
	rendered := append(slices.Clone(environment), mounts...)
	if len(rendered) < renderedIssueVolumePaths {
		t.Errorf("the renderer puts %d paths on the issue's volume, want at least %d: %v. The set is read "+
			"out of the renderer, so a derivation that matched nothing would hold the trimmer's two lists to "+
			"an empty set and report agreement", len(rendered), renderedIssueVolumePaths, rendered)
	}
	// base is what the rule earns over the rendered set alone, and the three controls below are
	// read against it rather than against a whole list: a control that asked for one complaint
	// outright would also fail for a defect of the lists themselves, where it has nothing to say.
	base := trimmerListComplaints(protected, rendered)
	for _, complaint := range base {
		t.Error(complaint)
	}

	// Every cache the pod's own install blocks render under the cache home is on the protected
	// list. The cache home's immediate children are each reclaimable but for its two exceptions,
	// so an install of ours that writes one and is not excepted is a cache the trimmer may delete
	// under the `flock` the install holds on it — the failure claudeCodeBlock's own exception
	// exists to prevent, and a third Legion install there would land eligible with nothing red.
	cacheHome := roleCacheHome(t, role)
	script := renderedStartWrapper(t, role)
	installs, unprotected := cacheHomeInstalls(script, cacheHome, protected)
	if len(installs) == 0 {
		t.Errorf("the rendered start wrapper names no $XDG_CACHE_HOME path at all, though claudeCodeBlock "+
			"renders `cache=\"$XDG_CACHE_HOME/legion-claude-code\"`: the read found nothing, which is a broken "+
			"read and not agreement. Script:\n%s", script)
	}
	for _, dir := range unprotected {
		t.Errorf("the pod's install blocks render the cache %s, which ProtectedPaths does not name: the cache "+
			"home's children are reclaimable but for its exceptions, so the trimmer may delete this one under "+
			"the lock its install holds on it", dir)
	}

	// The three negative controls. Each extends a copy of what the renderer gave rather than
	// editing the renderer, since the renderer is where the real addition lands and this test must
	// be the thing that reports it. A rule that stopped reporting any of the three would pass the
	// head that makes that addition, which is the whole failure this test exists against.
	//
	// 1. A cache added to the renderer under the cache home. A reclaimable root covers it and no
	// list names it, so the reading that counts coverage reports nothing here.
	reportsOnly(t, "a cache the renderer added under the cache home", base,
		trimmerListComplaints(protected, append(slices.Clone(rendered), cacheHome+"/"+newToolCache)), cacheHome+"/"+newToolCache)

	// 2. A mount added to the volume, named by neither list. An environment-only source would miss
	// it: the volume's own mounts and constants are named by no variable, which is why the set
	// this test walks is wider than issueVolumeDirectories, and the three it adds are each a path
	// that source does not hold.
	for _, mount := range mounts {
		if slices.Contains(environment, mount) {
			t.Errorf("%s is already one of the renderer's environment values, so it is no evidence that the "+
				"set reaches what the environment never names", mount)
		}
	}
	reportsOnly(t, "a mount added to the volume", base,
		trimmerListComplaints(protected, append(slices.Clone(rendered), newMount)), newMount)

	// 3. A Legion install block added under the cache home with its path on neither list, driven
	// through the rendered text the first half reads.
	_, withTheNewOne := cacheHomeInstalls(script+"\n  cache=\"$XDG_CACHE_HOME/"+newAgentCache+"\"\n", cacheHome, protected)
	if len(withTheNewOne) != len(unprotected)+1 || !slices.Contains(withTheNewOne, cacheHome+"/"+newAgentCache) {
		t.Errorf("a Legion install block rendering $XDG_CACHE_HOME/%s leaves the unprotected caches at %v, want "+
			"the one more naming %s: an install block added under the cache home and named by no list must turn "+
			"this test red", newAgentCache, withTheNewOne, cacheHome+"/"+newAgentCache)
	}
}

// issueVolumePaths is every path the pod renderer puts on the issue's volume, in two halves so
// that each is visible as what it is. environment is the renderer's own values, as
// issueVolumeDirectories (reclaim_doc_test.go) takes them from the start environment
// (mainEnvironment). mounts is what the volume carries beside them and no environment variable
// names: the pod's feed, which workspace-fetch writes and every other container mounts read-only
// (FeedDir); the shared clone every workspace is cut from (workspace.CloneRoot); and the
// workspaces root, derived from workspace.Location — the issue's checkout is
// <root>/workspaces/<owner>/<repo>/<issue>, so its directory three levels up is that root —
// rather than written out here, so the layout stays the one source of it.
func issueVolumePaths(t *testing.T, issue string, role claim.Role) (environment, mounts []string) {
	t.Helper()
	repository, err := ghrepo.Parse("workspace repository", "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	located, err := workspace.Location(TreeRoot, repository, issue)
	if err != nil {
		t.Fatal(err)
	}
	workspacesRoot := path.Dir(path.Dir(path.Dir(located.Dir)))
	return issueVolumeDirectories(t, issue, role), []string{FeedDir, workspace.CloneRoot(TreeRoot), workspacesRoot}
}

// trimmerListComplaints is the rule, as one complaint per path the two lists fail: it is a
// function rather than a loop of t.Errorf so that the negative controls can run it over a set of
// their own and hold it to reporting what they added.
func trimmerListComplaints(protected map[string]bool, paths []string) []string {
	var complaints []string
	for _, dir := range paths {
		reclaimable := slices.ContainsFunc(launcher.ReclaimableRoots, func(root launcher.ReclaimableRoot) bool {
			return root.Path == dir
		})
		switch {
		case reclaimable && namesPathOrAncestor(protected, dir):
			complaints = append(complaints, "the trimmer's lists both reclaim and protect "+dir+
				": its entries are swept and nothing at or under it may be deleted, so which of the two the "+
				"trimmer does is read out of the code rather than out of the lists")
		case !reclaimable && !namesPathOrAncestor(protected, dir):
			complaints = append(complaints, "neither of the trimmer's lists names "+dir+
				", though the pod renderer puts it on the issue's volume: add it to ReclaimableRoots as that "+
				"path itself, or to ProtectedPaths as that path or an ancestor of it. Sitting under a "+
				"reclaimable root is not being named by one, since only part of such a root is eligible")
		}
	}
	return complaints
}

// protectedList is ProtectedPaths as namesPathOrAncestor reads a list, which is the one reading of
// "an ancestor names it" this package keeps (reclaim_doc_test.go): a set of paths, each naming a
// rendered path when it is that path or sits above it, and TreeRoot itself naming nothing.
func protectedList() map[string]bool {
	named := make(map[string]bool, len(launcher.ProtectedPaths))
	for _, p := range launcher.ProtectedPaths {
		named[p] = true
	}
	return named
}

// cacheHomeVariable matches the cache home as the rendered install blocks name it — a shell
// expansion of XDG_CACHE_HOME followed by one path segment, the form claudeCodeBlock writes
// (`cache="$XDG_CACHE_HOME/legion-claude-code"`) and the form installBlock would write if it read
// that variable instead of BUN_INSTALL_CACHE_DIR. The braced spelling is matched too, since
// nothing in the script decides between them.
var cacheHomeVariable = regexp.MustCompile(`\$\{?XDG_CACHE_HOME\}?/([A-Za-z0-9._-]+)`)

// cacheHomeInstalls is every cache path the rendered start wrapper puts under the pod's cache
// home, and the ones of those the protected list does not name. The cache home is the renderer's
// own value for the variable rather than a path written here, so the two halves cannot drift.
func cacheHomeInstalls(script, cacheHome string, protected map[string]bool) (rendered, unprotected []string) {
	for _, match := range cacheHomeVariable.FindAllStringSubmatch(script, -1) {
		dir := cacheHome + "/" + match[1]
		if slices.Contains(rendered, dir) {
			continue
		}
		rendered = append(rendered, dir)
		if !namesPathOrAncestor(protected, dir) {
			unprotected = append(unprotected, dir)
		}
	}
	return rendered, unprotected
}

// roleCacheHome is the XDG_CACHE_HOME the renderer tells a role, read out of roleEnvironment: the
// install blocks expand that variable and nothing else, so the path this test resolves them
// against is the one the pod sets.
func roleCacheHome(t *testing.T, role claim.Role) string {
	t.Helper()
	for _, env := range roleEnvironment(role) {
		if env.Name == "XDG_CACHE_HOME" {
			return env.Value
		}
	}
	t.Fatalf("roleEnvironment(%s) sets no XDG_CACHE_HOME, so the pod's install blocks have no cache home to "+
		"expand and this test has nothing to resolve them against", role)
	return ""
}

// renderedStartWrapper is the start script a launch of this pod renders, built as launcherCommand
// builds it (launcher.go): the role's own state home and generation, the plugin pair's install
// block, Claude Code's — the one that writes under the cache home today — and the skills block, so
// a cache path added to any of the three is in the text this test reads. An Oh My Pi launch
// renders no Claude block, which is why the launch rendered here is a Claude one.
func renderedStartWrapper(t *testing.T, role claim.Role) string {
	t.Helper()
	r := skillsRuntime(t)
	return startScript(roleStateHome(role), 3, testPlugins(), claudeConfigDir(role),
		skillsInstallation(r.skills, r.tools.Legion), installLockWait(r.bootTimeout, r.registrationIntervals))
}

// reportsOnly holds a negative control's run of the rule to one complaint more than the rendered
// set already earns, naming the path that control added: a control that passed because the rule
// complains about everything would prove nothing about the addition it stands for, and one read
// against base says nothing about a defect of the lists themselves, which the walk above reports.
// The added path is last because the control appends it, and the rule keeps the order of the
// paths it is given.
func reportsOnly(t *testing.T, control string, base, complaints []string, added string) {
	t.Helper()
	if len(complaints) != len(base)+1 || !strings.Contains(complaints[len(complaints)-1], added) {
		t.Errorf("over the rendered set plus %s the rule reports %v, where the set alone earns %v: want the one "+
			"further complaint naming %s, since with the rule silent here the change that makes that addition to "+
			"the renderer passes", control, complaints, base, added)
	}
}
