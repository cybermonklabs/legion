/**
 * The surface of `dispatch login --device`, a command of the `dispatch` command line that no
 * Dispatch tool spec defines: its name, the sentence `dispatch --help` lists and its help. The
 * command itself is `dispatch-commands/login.ts`.
 *
 * This module imports nothing: the docs generator's program is `dispatch-command.ts`'s, which
 * imports this module and holds no `@legion/cipher-client` source (`.github/scripts/ci_job_reads.py`,
 * the docs gate's read of envoy-client's sources). So the help writes out the variables and the
 * command that `@legion/cipher-client` names, the scopes a Dispatch sign-in may ask for
 * (`DISPATCH_SIGN_IN_SCOPES`) and the reason's bound (`BROKER_REQUEST_REASON_MAX`) that
 * `@legion/contracts` names, and `__tests__/device-login.test.ts` holds each to its constant.
 */

/** The login's command word. */
export const LOGIN_COMMAND = "login";

/** What `dispatch --help` lists for the login: the first sentence of its help. */
export const LOGIN_SENTENCE =
  "Logs this machine in at the secrets broker, once per machine, for a machine with no identity of its own, such as a laptop.";

/** What `dispatch login --help` prints. */
export function loginHelp(): string {
  return [
    "Usage: dispatch login --device [--person <address>] [--scope <list>] [--reason <text>]",
    "",
    `${LOGIN_SENTENCE} With --scope, it makes a Dispatch sign-in for a session or tool instead: the person approves the scopes it asks for, ticking some or all of them, and its tokens hold those scopes alone, never approve; it enrolls no session and reads no secret. Whatever is ticked, the session writes in Dispatch and works on Envoy as its own session within its person's projects; the scope person adds acting as the person, the scope p0, which the page shows unticked until the person ticks it, adds making an issue's own priority 0, and --scope none asks for no scope. A broker serves Dispatch sign-ins only where its deployment turns them on. It prints a code and the page to type it into; the person the login names approves it there, signed in to Dispatch. The login is then kept in the file AGENT_SECRETS_DEVICE_LOGIN_FILE names (default ~/.config/legion/device-login.json), mode 0600, in place of any login that file held, and every session started after that sets AGENT_SECRETS_MACHINE_IDENTITY=device-login trades it for its broker token. A session already running refuses to trade a new login, a scope change included: start a new session to trade it, since the broker keeps a session's name for the identity that wrote under it, and the login file does not say whose the new login is, so that session started again may be refused. To keep a machine login and a Dispatch sign-in on one machine, name another file for one of them.`,
    "",
    "Flags:",
    "  --device",
    "      Log in with the broker's device grant: the only way there is, and required.",
    "  --person <address>",
    "      The person who approves the login, by the address they sign in to Dispatch with; else AGENT_SECRETS_PERSON.",
    "  --scope <list>",
    "      Create a Dispatch sign-in for the comma-separated scopes person, p0, or none. Omit it for a machine login.",
    "  --reason <text>",
    "      Name the session or tool. It may have at most 400 characters.",
    "",
    "It reads AGENT_SECRETS_URL, the broker, and needs no Dispatch host or session.",
    "",
    "It exits 0 once the login is kept; 1 when the broker could not be reached or refused it, the person denied it, its code expired, or the login could not be kept; and 2 on a usage error, before any request.",
    "",
    "Example: dispatch login --device --person ada@example.com --scope person",
    "",
  ].join("\n");
}
