// packages/cipher-client/src/machine-audience.ts
//
// The variable that names the audience of this machine's identity token. It imports nothing, so
// `environment.ts`, which re-exports it, and the identities `environment.ts` builds can each take
// it without an import cycle.

/** The audience of this machine's identity token: the metadata server mints it for that
 *  audience, a pool mint names it as the minted token's own, and GitHub mints a GitHub Actions
 *  run's for it. */
export const MACHINE_AUDIENCE_VAR = "AGENT_SECRETS_MACHINE_AUDIENCE";
