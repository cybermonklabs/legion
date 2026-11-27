/**
 * The surface of `dispatch login --device`, a command of the `dispatch` command line that no
 * Dispatch tool spec defines: its name, the sentence `dispatch --help` lists and its help. The
 * command itself is `dispatch-commands/login.ts`.
 *
 * This module imports nothing: the docs generator's program is `dispatch-command.ts`'s, which
 * imports this module and holds no `@legion/cipher-client` source (`.github/scripts/ci_job_reads.py`,
 * the docs gate's read of envoy-client's sources). So the help writes out the variables and the
 * command that `@legion/cipher-client` names, and `__tests__/device-login.test.ts` holds each to
 * that package's constant.
 */

/** The login's command word. */
export const LOGIN_COMMAND = "login";

/** What `dispatch --help` lists for the login: the first sentence of its help. */
export const LOGIN_SENTENCE =
  "Logs this machine in at the secrets broker, once per machine, for a machine with no identity of its own, such as a laptop.";

/** What `dispatch login --help` prints. */
export function loginHelp(): string {
  return [
    "Usage: dispatch login --device [--person <address>]",
    "",
    `${LOGIN_SENTENCE} It prints a code and the page to type it into; the person the login names approves it there, signed in to Dispatch. The login is then kept in the file AGENT_SECRETS_DEVICE_LOGIN_FILE names (default ~/.config/legion/device-login.json), mode 0600, and every session that sets AGENT_SECRETS_MACHINE_IDENTITY=device-login trades it for its broker token.`,
    "",
    "Flags:",
    "  --device",
    "      Log in with the broker's device grant: the only way there is, and required.",
    "  --person <address>",
    "      The person who approves the login, by the address they sign in to Dispatch with; else AGENT_SECRETS_PERSON.",
    "",
    "It reads AGENT_SECRETS_URL, the broker, and needs no Dispatch host or session.",
    "",
    "It exits 0 once the login is kept; 1 when the broker could not be reached or refused it, the person denied it, its code expired, or the login could not be kept; and 2 on a usage error, before any request.",
    "",
    "Example: dispatch login --device --person ada@example.com",
    "",
  ].join("\n");
}
