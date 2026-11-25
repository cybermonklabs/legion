/** Whether this run's Dispatch server reads, for a service account, the routes
 * `DISPATCH_ACCESS_ACCOUNTS` gates (the membership answer and the repository answer, listed in
 * `packages/envoy/cmd/dispatch/README.md`), from one switch, `DISPATCH_E2E_ACCESS_ISSUER`, which
 * `e2e/run-server.sh` reads the same way:
 *
 * - unset, or any value but `1`: the server verifies no service-account token, so those routes
 *   refuse every caller, as a deployment that names no access account does. Every spec runs this
 *   way;
 * - `1`: the harness starts `packages/envoy/cmd/dev-oidc-issuer` (`e2e/run-access-issuer.sh`)
 *   before the server. It keeps the secrets broker's projected service-account token in the file
 *   `DISPATCH_E2E_ACCESS_TOKEN_FILE` names, a path in the harness's own directory that the harness
 *   exports with the ports (`e2e/harness-start.ts`). The server verifies that issuer's tokens for
 *   the audience `e2e/access-account.sh` names, and lists that file's account in
 *   `DISPATCH_ACCESS_ACCOUNTS`, so a driver may read those routes with the file's token. The one
 *   that does, `e2e/acceptance/membership-answer.sh`, reads the membership answer's routes; no
 *   driver reads the repository answer.
 *
 * A deployed run (`PLAYWRIGHT_BASE_URL`) starts no server, so it starts no issuer either. This
 * module only reads the environment, so any process may import it.
 */
export const usesAccessIssuer =
  process.env.DISPATCH_E2E_ACCESS_ISSUER === "1" && !process.env.PLAYWRIGHT_BASE_URL;
