package sandbox

// ghprojection.go is the GitHub projection model of a pod: what each role's gh volume projects
// (ghProjection, ghProjectionOf), the drift check that compares it with what a pod created now
// would project (movedGhDirs, which driftedInPod asks), and the directory helpers both read. The
// refresher reads the same model (ghcredential.go's servedLayout).

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// movedGhDirs is the owner directories a pod created now would project (r.gitHubDirs,
// appauth.GitHubDirs) that role's gh volume of the pod projects none of; ok is whether it lacks
// any. That volume is one Secret projection whose items are fixed when the pod is created
// (volumes), one pair of keys per directory of the launch, while the role Secret those keys come
// from gains a directory's pair the moment the daemon serves it (refreshGitHubCredentials). So a
// Secret that holds a new owner's files reaches no pod made before it: the pod's
// /var/run/legion/gh has no directory of that name, every `gh` call of its roles that would read
// the token there runs unauthenticated, and only a new pod projects it.
//
// What a pod created now would project is what a mint really produces, not what the Apps are
// installed on: an owner whose every repository GitHub refuses a lease for is left out of the
// bundle and recorded as left out (appauth's mintBundle and LeftOut), so appauth.GitHubDirs leaves
// it out of this answer too, and a pod without it is no pod to replace. Were it named here, the
// pod would be replaced at every sweep and its replacement's bundle would lack the directory
// again, for ever. A mint that answers for it again clears the record, and the pod
// is then replaced once, which is right.
//
// That record is read for the Apps this pod runs and no others: the roles of the kind the role
// belongs to (podKindFor), mapped through appauth.AppRoleFor. An issue pod projects what both
// Apps' bundles hold, so a directory either App is refused is in no projection of it; the
// project controller's pod holds the review App's bundle alone, and an owner the implement
// App's installation does not reach is one its own `gh` reaches all the same, so a controller
// pod without that owner's directory has drifted and is replaced by its next launch.
//
// Owner directories alone are compared, on both sides (ghOwnerDirs). A bundle this daemon
// builds holds none but owner directories: a lease reaches every repository its installation
// holds, so there is nothing narrower for a directory to stand for (appauth.GitHubCredential).
// A pod holding a `<owner>/<repo>` directory was launched before that change and
// has drifted on that alone — the names ghOwnerDirs drops are the second reason below, which
// names the directory — since its gh holds a token that reaches one repository where the
// bundle of a pod created now holds its owner's, which reaches every repository of that
// installation.
// An owner's own directory is the first reason: a pod without it holds no token of that owner
// at all.
//
// The bundle's pairs file is the third reason, and the one read by its presence alone
// (GitHubPairsKey, ghconfig.PairsFile): every bundle of this daemon holds it, empty where the
// project has no configured rename that resolves, and `legion gh` and `legion git-credential`
// route a moved repository by it. A pod created before it existed projects no item for it, and
// a projection's items are fixed for the pod's life, so that pod's wrappers would route by
// owner alone for as long as it runs and no Update could mend it. Its content is never
// compared: the kubelet rewrites a projected file in place when the Secret changes, so a pair
// configured since the launch reaches a kept pod (refreshGitHubCredential), and a pod is
// replaced for what a refresh cannot reach and for nothing else.
//
// The pod's own repository is still read from its workspace fetch (fetchRepository), and a pod
// that fetches none — the controller's — asked for with the zero Repository, since the owner
// directories a bundle holds depend on it: among them is the owner the pod writes to, which is
// the role's own and not the project's.
//
// The two pods without a Secret projection are not the same. One with no gh volume at all is no
// pod of this runtime's making and stays unmoved: this runtime does not know what it holds. One
// whose gh volume is an EmptyDir was launched with no directories at all (volumes: a Secret
// projection with no items would project every key of the Secret, the launcher token among
// them), so it holds the empty set — a controller whose project had no owner yet reads every
// owner the daemon serves now as lacking, and the first owner its project gains remakes it.
// The comparison runs one way only: a directory the pod holds that the daemon no longer serves
// costs the pod a stale directory and nothing more, and must never be worth remaking a pod and
// deleting its disk for. The daemon's set comes sorted (Options.GitHubDirs), and the pod's is
// sorted here, so each is searched and named in one order.
func (r *Runtime) movedGhDirs(pod *corev1.Pod, role claim.Role) (movedAddress, bool) {
	name := roleVolume(ghVolume, role)
	projected, found := ghProjectionOf(pod, role)
	if !found {
		return movedAddress{}, false
	}
	if r.proxiesGitHubIn(nil, role) {
		if projected.secret {
			return movedAddress{where: name + " projects the role's App tokens", held: ghDirsName(ghOwnerDirs(projected.dirs)),
				handed: "no token: the role's forwarder's placeholders (runtime.kubernetes.agent_secrets.github)"}, true
		}
		return movedAddress{}, false
	}
	held := projected.dirs
	slices.Sort(held)
	owned := ghOwnerDirs(held)
	// A `<owner>/<repo>` directory is no bundle of this daemon's: the pod was launched by one
	// that still cut a directory per repository, and its token reaches that repository where
	// the owner's own directory of a bundle built now reaches every repository of the
	// installation. The names ghOwnerDirs drops are exactly those.
	if repositories := slices.DeleteFunc(slices.Clone(held), func(dir string) bool { return !strings.Contains(dir, "/") }); len(repositories) > 0 {
		return movedAddress{where: name + " holds a repository directory", held: ghDirsName(repositories),
			handed: "the owner directories alone"}, true
	}
	// A pod launched with no directory at all projects no Secret and so no pairs file either,
	// and a launch now would hand it the same empty directory: it is compared for its owner
	// directories alone, below.
	if projected.secret && !projected.pairs {
		return movedAddress{where: name + " holds no " + ghconfig.PairsFile, held: ghDirsName(owned),
			handed: "the same directories beside the configured renames the wrappers route by"}, true
	}
	repo, _ := fetchRepository(pod)
	served := ghOwnerDirs(r.gitHubDirs(repo, podAppRoles(podKindFor(role))))
	lacking := slices.ContainsFunc(served, func(dir string) bool {
		_, found := slices.BinarySearch(owned, dir)
		return !found
	})
	if !lacking {
		return movedAddress{}, false
	}
	return movedAddress{where: name, held: ghDirsName(owned), handed: ghDirsName(served)}, true
}

// ghProjection is what one role's gh volume of a pod projects, read off its items, which are
// fixed for the pod's life: dirs, the bundle directories they name (GitHubHostsDir), in the order
// they come, pairs, whether they hold the bundle's pairs file (GitHubPairsKey), and secret,
// whether the volume is a Secret projection at all — not one of a pod launched with no directory,
// whose gh volume is an empty directory projecting nothing. The drift check reads it (movedGhDirs)
// and so does the refresher, which serves a pod the layout it names (servedLayout).
type ghProjection struct {
	dirs          []string
	pairs, secret bool
}

// ghProjectionOf is what role's gh volume of pod projects, and whether pod has such a volume at
// all: one with none is no pod of this runtime's making. It reads the volume, never its mount, so
// it reads the controller's pod in either of its layouts: the single-role form mounts the volume
// at GHConfigDir and the roles-dir form under the controller's directory of RolesDir.
func ghProjectionOf(pod *corev1.Pod, role claim.Role) (ghProjection, bool) {
	name := roleVolume(ghVolume, role)
	at := slices.IndexFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
	if at < 0 {
		return ghProjection{}, false
	}
	source := pod.Spec.Volumes[at].Secret
	if source == nil {
		return ghProjection{}, true
	}
	projected := ghProjection{secret: true}
	for _, item := range source.Items {
		if item.Key == GitHubPairsKey {
			projected.pairs = true
		}
		if dir, ok := GitHubHostsDir(item.Key); ok {
			projected.dirs = append(projected.dirs, dir)
		}
	}
	return projected, true
}

// podAppRoles are the GitHub Apps a pod of kind holds a bundle of, each named once
// (appauth.AppRoleFor): both of them for an issue pod, whose six roles run as one or the other,
// and the review App alone for the project controller's. It is what the record of the
// directories a mint found GitHub refuses is read for (movedGhDirs, appauth.LeftOut).
func podAppRoles(kind podKind) []appauth.AppRole {
	apps := make([]appauth.AppRole, 0, len(appauth.Roles))
	for _, role := range kind.roles() {
		if app := appauth.AppRoleFor(role); !slices.Contains(apps, app) {
			apps = append(apps, app)
		}
	}
	return apps
}

// PodAppRoles is podAppRoles of the pod role runs in (podKindFor): both Apps for a workflow role,
// whose issue pod projects the directories every role's bundle holds and no more (gitHubBundles,
// volumes), and the review App alone for the project controller. A role's container holds only
// the directories of that projection, so a caller that says which owners a role's token reaches
// in a pod of this runtime reads them for these Apps together (internal/daemon's plan.appOwners),
// as the drift check does (movedGhDirs).
func PodAppRoles(role claim.Role) []appauth.AppRole { return podAppRoles(podKindFor(role)) }

// ghOwnerDirs is the owner directories of a gh directory set, in the order they come: the names
// holding no slash, where a repository's directory a pod launched before bundles held owner
// directories alone projects is
// `<owner>/<repo>`. It is what movedGhDirs compares, a repository directory a pod lacks being no
// address of its own.
func ghOwnerDirs(dirs []string) []string {
	owners := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !strings.Contains(dir, "/") {
			owners = append(owners, dir)
		}
	}
	return owners
}

// ghDirsName names a set of gh directories as one string, each name in order; "(unset)" for none,
// and no credential in any of it.
func ghDirsName(dirs []string) string {
	if len(dirs) == 0 {
		return "(unset)"
	}
	return strings.Join(dirs, " ")
}
