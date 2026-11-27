/**
 * Logs each warning the Envoy listener answered a granted claim of `role` for `sessionID` with
 * (`SetRoleResult.warnings`), one `log` line each, naming the role and the session: what the
 * listener says about a claim that landed, such as a handover of the replaced session it did not
 * make. Every claim the channel server makes logs them, whoever made it: a restore, a heartbeat,
 * the Legion claim, or `envoy_role_set`, which also answers them to the agent.
 */
export function logRoleClaimWarnings(
  log: (line: string) => void,
  role: string,
  sessionID: string,
  warnings: readonly string[],
): void {
  for (const warning of warnings) {
    log(
      `the Envoy listener warned about the claim of role ${role} for session ${sessionID}: ${warning}`,
    )
  }
}
