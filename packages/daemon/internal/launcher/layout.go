package launcher

// The layout of an issue pod's roles directory (--roles-dir), which the pod builder
// (internal/runtime/sandbox) mounts and `legion launcher` and its role-init read: one directory per
// role, holding the role's launcher token projection (RoleLauncherDir, its token file
// LauncherTokenFile), its gh bundle (RoleGHDir), and its own memory-backed private, state and
// agent-secrets directories (RolePrivateDir, RoleStateDir, RoleAgentSecretsDir); and RoleHomesDir,
// the one disk directory, which holds each role's home overlay layers, its own temporary
// directories and its own config home (RoleConfigHomeDir). Both sides name these constants, so a
// renamed directory moves on both.
const (
	RoleLauncherDir     = "launcher"
	RoleGHDir           = "gh"
	RolePrivateDir      = "private"
	RoleStateDir        = "state"
	RoleAgentSecretsDir = "agent-secrets"
	RoleHomesDir        = ".homes"
	RoleConfigHomeDir   = "config"
	LauncherTokenFile   = "LAUNCHER_TOKEN"
)

// SessionWriterDirName is the directory under the roles directory that the daemon mounts the
// session writer token in, and SessionWriterTokenFile is the token's file in it. The writer token
// is the projected ServiceAccount token whose audience is the session database's writer audience
// (ompsessions.Database.WriterAudience): the session proxy exchanges it to impersonate the
// session database's service account. So its path is <roles-dir>/session-writer/token. role-init
// covers the whole roles directory with an empty filesystem (the covers of prepareRoleNamespace,
// roleinit_linux.go), so no role reads the token. The launcher gives the path to the proxy alone:
// it writes the path into no generation's environment and no file.
const (
	SessionWriterDirName   = "session-writer"
	SessionWriterTokenFile = "token"
)

// The paths a role sees inside its own namespaces. FixedRoot is where `legion launcher role-init`
// binds the role's own gh, private, state and agent-secrets directories back, at these same
// names, which are the paths every role is told (internal/runtime/sandbox's GHConfigDir and its
// siblings): the worker image makes each an empty root-owned mount point. Home is the image's
// home directory, which each role's namespace overlays with a writable layer of its own.
// HomeMountsDir is where an issue pod's roles container mounts every volume that belongs at or
// under Home — the shared config home, an operator's mount — at the same
// path relative to it, so Home itself carries no mount and the overlay's lower layer has none
// under it (Linux refuses an overlay whose lower layer has mounts locked under it from a more
// privileged namespace); role-init binds each back at its own path under Home once the overlay
// is in place, and covers HomeMountsDir, so the role sees each mount only where it belongs.
//
// ConfigHomeDir is the XDG config home's place under Home, the XDG_CONFIG_HOME every process of an
// issue pod is told. The pod's one shared config home, which workspace-init writes Legion's jj
// configuration of the issue's clone into, is mounted there in workspace-init and under
// HomeMountsDir in the roles container; no role sees it. role-init binds each role's own config
// home at that path instead (RoleConfigHomeDir under the role's directory of RoleHomesDir), made
// once per pod as a copy of the shared one, so what one role writes there — a git `core.fsmonitor`
// or a jj alias, which run in whatever git or jj reads them — never reaches another role.
//
// RoleTestPGLog is the file under FixedRoot at which role-init binds, read-only, the log of the
// pod's own test database (testpg.go), the one file from under the roles directory every role of
// the pod reads: the worker image makes it an empty root-owned file, the mount point.
const (
	FixedRoot     = "/var/run/legion"
	Home          = "/home/legion"
	HomeMountsDir = FixedRoot + "/home"
	ConfigHomeDir = ".config"
	RoleTestPGLog = "testpg.log"
)
