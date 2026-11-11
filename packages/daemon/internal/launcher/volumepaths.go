package launcher

// VolumeRoot is the issue pod's volume, the one filesystem the cache trimmer reads and the root
// of every path below it: the pod's own generic ephemeral volume, mounted whole at this path in
// the roles container (internal/runtime/sandbox's TreeRoot). The trimmer carries it as a constant
// of its own, as it carries every path below, because the launcher's process is told none of
// them: a roles container's own environment is the pod's uid and nothing else, and TMPDIR,
// GOCACHE, XDG_CACHE_HOME and BUN_INSTALL_CACHE_DIR reach a role only in the start environment
// the daemon renders for each generation (roleEnvironment, internal/runtime/sandbox/manifest.go),
// which this process never holds. Reading them out of a generation's environment instead would
// make what the trimmer may delete depend on which role started last, and a pod whose roles have
// not started yet is exactly the pod whose caches are already full from the generation before.
const VolumeRoot = "/legion"

// EntryRule says what one entry of a reclaimable root is.
type EntryRule string

const (
	// GoBuildTrees: one `go-build*` directory of the root, removed whole. Go names these and
	// owns them, and makes one for every `go build` and every `go test`, because the pod sets
	// no GOTMPDIR.
	GoBuildTrees EntryRule = "go-build tree"
	// GoCacheEntries: one name ending `-a` or `-d` inside a shard directory of the root, with
	// its own modification time, deleted on its own; one that is a directory is removed whole.
	GoCacheEntries EntryRule = "go build cache entry"
	// ImmediateChildren: one immediate child of the root.
	ImmediateChildren EntryRule = "immediate child"
)

// ReclaimableRoot is one root the trimmer may delete entries of. Except names immediate
// children that are never entries, by base name.
type ReclaimableRoot struct {
	Path   string
	Entry  EntryRule
	Except []string
}

// The two children of the pod's cache home that are never entries, by base name.
const (
	// playwrightRegistryDir is the Playwright browser registry. It is named by nothing in the Go
	// tree: it is playwright-core's own name for `$XDG_CACHE_HOME/ms-playwright`, which the
	// issue's roles share because they share the cache home, recorded in docs/kubernetes.md's
	// pod anatomy ("Sharing the cache home gives the issue's roles one Playwright browser
	// registry, since playwright-core resolves it to `$XDG_CACHE_HOME/ms-playwright`"). No test
	// can derive it from the renderer, so the citation lives here with the constant.
	playwrightRegistryDir = "ms-playwright"
	// claudeCodeCacheDir is the npm cache a Claude Code launch installs its release through,
	// `$XDG_CACHE_HOME/legion-claude-code`, which the rendered start script takes a `/usr/bin/flock`
	// on for the whole of `npm ci` (claudeCodeBlock, internal/runtime/sandbox/plugins.go).
	claudeCodeCacheDir = "legion-claude-code"
)

// ReclaimableRoots are the four roots of the pod's volume whose entries the cache trimmer may
// delete, in the order it sweeps them. Each root itself is never unlinked — a role's tool makes
// its cache directory once and locks it, and the trimmer removing the inode under a waiting
// `flock` would hand two roles the same lock — and nothing outside these four is touched at all:
// the workspaces, the shared clone, the feed, the state and data homes, the module cache and
// uv's cache and Pythons are a role's work or a role's own state, and are named in
// ProtectedPaths so that a reader of either list can see the whole volume accounted for.
var ReclaimableRoots = []ReclaimableRoot{
	// $TMPDIR, of which only Go's own build trees are eligible. A role's clones, toolchains,
	// scratch databases and half-written artefacts live here too, under names the role chose,
	// and no program can tell a role's cache from a role's work by looking at them: a sweep of
	// this directory's other entries is exactly the hand deletion that lost a role's work
	// mid-round. A `go-build*` directory is the one entry Go names and owns, and it makes one
	// for every `go build` and every `go test` because the pod sets no GOTMPDIR.
	{Path: VolumeRoot + "/scratch/tmp", Entry: GoBuildTrees},
	// GOCACHE, swept the way Go's own trim sweeps it: "Remove only cache entries (xxxx-a and
	// xxxx-d)" inside a shard directory, each judged by its own modification time taken an hour
	// further back than the minimum age, as Go's own trim subtracts an mtimeInterval from its
	// cutoff for the imprecision of those times, and with a RemoveAll for the directory form
	// (trimSubdir and trim, cmd/go/internal/cache/cache.go; goCacheMtimeSlack, volumetrim.go).
	// The two kinds are named from an action id and an output id, each sharding on its own first
	// byte, so there is no pair to delete together: measured in this pod on 2026-11-10, `find
	// /legion/go/build -mindepth 2 -maxdepth 2 -type d` found no directory-form entry at all,
	// against 11,331 names ending `-a` and 3,634 ending `-d`, with no name in both sets. The
	// cache's own `trim.txt` and `README` sit in the cache root rather than in a shard, so a
	// rule that reaches only into shards never reaches them.
	{Path: VolumeRoot + "/go/build", Entry: GoCacheEntries},
	// XDG_CACHE_HOME, whose immediate children are each a whole tool's cache that the next run
	// rebuilds. The two exceptions are the children whose loss is not a rebuild: the Playwright
	// browser registry is a 400 MB download under an exclusive lock with about ten minutes of
	// back-off, so losing it stalls every other role of the pod that wants a browser
	// (docs/kubernetes.md's pod anatomy), and a Claude Code launch takes a `flock` on its npm
	// cache and is tried once, so a cache deleted under it is the launch's failure and not a
	// slower launch (claudeCodeBlock, internal/runtime/sandbox/plugins.go).
	{
		Path: VolumeRoot + "/scratch/cache", Entry: ImmediateChildren,
		Except: []string{playwrightRegistryDir, claudeCodeCacheDir},
	},
	// BUN_INSTALL_CACHE_DIR, whose immediate children a `bun install` refills from the registry.
	// The root itself is the live `flock` target of every rendered install (installBlock,
	// internal/runtime/sandbox/plugins.go), which is why no root of this list is ever unlinked.
	{Path: VolumeRoot + "/scratch/bun/install/cache", Entry: ImmediateChildren},
}

// ProtectedPaths are the paths of the pod's volume the trimmer never deletes anything at or
// under. Together with ReclaimableRoots they account for the whole volume, and that is the rule
// a test in internal/runtime/sandbox holds these two lists to: every path the pod renderer puts
// on the volume is named by one of the two lists — by the reclaimable list as that path itself,
// or by the protected list as that path itself or as an ancestor of it. Coverage by a
// reclaimable root never counts, because $TMPDIR and the cache home are roots of which only part
// is eligible: a renderer that put a new directory under either would otherwise be accounted for
// by a root that deletes none of it, or worse, by one that deletes all of it. A path added to
// the volume must therefore be added to one of these lists by hand, which is the point: the edit
// that forgets it fails that test rather than losing a role's work.
var ProtectedPaths = []string{
	// The issue's checkouts, one per issue: the work itself (workspace.Location).
	VolumeRoot + "/workspaces",
	// The shared clones every workspace is cut from, `/legion/repos/github.com/<owner>/<repo>`.
	VolumeRoot + "/repos",
	// The pod's feed, which workspace-fetch wrote and every other container mounts read-only
	// (sandbox.FeedDir).
	VolumeRoot + "/feed",
	// XDG_STATE_HOME's parent: one state home per role (roleStateHome), holding Oh My Pi's
	// profile, its logs and the browser broker's lock, none of it a cache.
	VolumeRoot + "/scratch/state",
	// XDG_DATA_HOME, where a tool keeps what it is meant to keep rather than what it can fetch
	// again.
	VolumeRoot + "/scratch/data",
	// GOMODCACHE, whose entries the go command makes read-only and verifies against a known
	// hash; it is a download the pod has no network budget to repeat for every sweep, and the
	// image's GOPROXY copy is what fills it.
	VolumeRoot + "/go/mod",
	// uv's cache, out of which uv copies a package into a workspace's .venv (uvCacheDir).
	VolumeRoot + "/uv/cache",
	// uv's Pythons, one directory per issue: a workspace's .venv links to an interpreter in it,
	// so deleting one breaks every environment of that issue (uvPythonRoot).
	VolumeRoot + "/uv/python",
	// The two children of the cache home that are the cache home's exceptions: named here as
	// well, so a reader of this list alone sees them kept and the accounting rule above holds
	// without reading the exceptions of a reclaimable root.
	VolumeRoot + "/scratch/cache/" + playwrightRegistryDir,
	VolumeRoot + "/scratch/cache/" + claudeCodeCacheDir,
}
