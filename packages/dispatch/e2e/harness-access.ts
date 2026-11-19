/** Whether this run's Dispatch server reads the membership answer for a service account, from one
 * switch, `DISPATCH_E2E_ACCESS_ISSUER`, which `e2e/run-server.sh` reads the same way:
 *
 * - unset, or any value but `1`: the server verifies no service-account token, so its three routes
 *   of the membership answer (`GET /api/v1/access/projects/{key}`, `GET /api/v1/access/person`,
 *   `GET /api/v1/access/private-projects`) refuse every caller, as a deployment that names no
 *   access account does. Every spec runs this way;
 * - `1`: the harness starts `packages/envoy/cmd/dev-oidc-issuer` (`e2e/run-access-issuer.sh`)
 *   before the server. It keeps the secrets broker's projected service-account token in the file
 *   `DISPATCH_E2E_ACCESS_TOKEN_FILE` names, a path in the harness's own directory that the harness
 *   exports with the ports (`e2e/harness-start.ts`). The server verifies that issuer's tokens for
 *   the audience `e2e/access-account.sh` names, and lists that file's account as the one that
 *   reads the membership answer, so a driver reads the three routes with the file's token
 *   (`e2e/acceptance/membership-answer.sh`).
 *
 * A deployed run (`PLAYWRIGHT_BASE_URL`) starts no server, so it starts no issuer either. This
 * module only reads the environment, so any process may import it.
 */
export const usesAccessIssuer =
  process.env.DISPATCH_E2E_ACCESS_ISSUER === "1" && !process.env.PLAYWRIGHT_BASE_URL;
