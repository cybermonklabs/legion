package api

// 20261125161436 role-identities: INFRA-4172 (cybermonklabs/legion) -- every workflow role's pane
// and pod is told LEGION_ROLE_IDENTITIES (runtime.RoleIdentitiesVariable), every role's git
// identity as JSON: one object from each role of claim.Roles to `{"name": …, "email": …}`, the
// identity that role's App commits as, keys sorted (runtime.RoleIdentities). The push step of
// `legion jj` reads it (runtime.ReadRoleIdentities) to set the author of a commit a push sends from
// the role its first `Legion-Role` trailer names, where the step's rules let it. The daemon puts it
// in every worker's spec.Env (specs.workerSpawnSpec), which the tmux runtime passes as a pane's -e
// pair and the Sandbox runtime in a role's start command; a failed mint of the claim's own App's
// lease fails the launch, and a failed mint of the other App's fails none: that launch's table
// holds its own App's roles alone, and the daemon logs a warning naming the claim, that App and the
// error. The controller's launch carries none, and `legion controller start` drops one it inherits.
// An operator's `runtime.tmux.pane_env`, `runtime.kubernetes.pod.env` and provider keys may not
// name it (runtime.PaneVariableRefusal; config's checkTmuxProviderKeyNames for a provider key under
// runtime tmux; sandbox's podVariables). A role process of an earlier release has no table, and its
// step does nothing; the reader passes over a field of an identity it does not know, so a field a
// later release adds turns no step off. No shape of `GET /legion/v1/state` or of a request moves,
// and this change adds no store migration, so the entry adds no row to any table: it is an entry
// because the variable is a surface the daemon and a role's process share, which both sides of one
// contract number must agree on.
const roleIdentitiesContract = 20261125161436

func init() {
	registerEntry(contractEntry{id: roleIdentitiesContract, slug: "role-identities"})
}
