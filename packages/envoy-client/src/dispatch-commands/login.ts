// packages/envoy-client/src/dispatch-commands/login.ts
//
// `dispatch login --device`: logs this machine in at the secrets broker's device grant, once per
// machine, for a machine with no platform identity of its own, such as a person's laptop. It asks
// the broker for a code, prints where to type it and the code, waits while the person approves
// it in Dispatch, and keeps the login in its file (`@legion/cipher-client/device-login`). After
// that, every session of the machine that sets `AGENT_SECRETS_MACHINE_IDENTITY=device-login`
// trades the login for its broker token.
//
// It is the one `dispatch` command that is no Dispatch tool: a person runs it in a terminal,
// before any session exists, so it reads no host, no session and no envoy.json, only the
// environment's broker settings.

import { hostname } from "node:os";
import {
  DEVICE_LOGIN_FILE_NAMED,
  deviceLoginBroker,
  deviceLoginFile,
  deviceLoginHost,
  deviceLoginPlace,
  finishDeviceLogin,
  readDeviceLogin,
  startDeviceLogin,
} from "@legion/cipher-client/device-login";
import {
  BROKER_URL_VAR,
  IDENTITY_DEVICE_LOGIN,
  IDENTITY_VAR,
} from "@legion/cipher-client/environment";
import { PERSON_VAR } from "@legion/cipher-client/signins";
import { DEVICE_LOGIN_COMMAND } from "@legion/cipher-client/token-errors";
import { isHttpUrl, urlForText } from "@legion/contracts/log-text";
import { messageFor } from "../errors";

/** The exit codes of every `dispatch` command: done, refused, and a usage error. */
const OK = 0;
const REFUSED = 1;
const USAGE = 2;

/** `dispatch login --help`. */
const LOGIN_HELP = [
  "Usage: dispatch login --device [--person <address>]",
  "",
  "Logs this machine in at the secrets broker, once per machine, for a machine with no identity of its own, such as a laptop. It prints a code and the page to type it into; the person the login names approves it there, signed in to Dispatch. The login is then kept in the file AGENT_SECRETS_DEVICE_LOGIN_FILE names (default ~/.config/legion/device-login.json), mode 0600, and every session that sets AGENT_SECRETS_MACHINE_IDENTITY=device-login trades it for its broker token.",
  "",
  "Flags:",
  "  --device",
  "      Log in with the broker's device grant: the only way there is, and required.",
  "  --person <address>",
  `      The person who approves the login, by the address they sign in to Dispatch with; else ${PERSON_VAR}.`,
  "",
  `It reads ${BROKER_URL_VAR}, the broker, and needs no Dispatch host or session.`,
  "",
  `Example: ${DEVICE_LOGIN_COMMAND} --person ada@example.com`,
  "",
].join("\n");

/** What the command writes to and waits with. */
export interface LoginIo {
  /** Writes one line or more of what the person reads. */
  print(text: string): void;
  readonly fetchImpl?: typeof fetch | undefined;
  /** Waits between two polls of the code; a timer unless a test replaces it. */
  readonly sleep?: ((ms: number) => Promise<void>) | undefined;
}

/** The flags `argv` sets, or the problem with them. */
type ParsedLogin =
  | { readonly kind: "help" }
  | { readonly kind: "login"; readonly device: boolean; readonly person: string | undefined }
  | { readonly kind: "refused"; readonly problem: string };

function parseLogin(argv: readonly string[]): ParsedLogin {
  let device = false;
  let person: string | undefined;
  for (let at = 0; at < argv.length; at += 1) {
    const word = argv[at] as string;
    if (word === "--help" || word === "-h") return { kind: "help" };
    if (word === "--device") {
      device = true;
    } else if (word === "--person") {
      const next = argv[at + 1];
      if (next === undefined) return { kind: "refused", problem: "--person needs an address" };
      person = next;
      at += 1;
    } else if (word.startsWith("--person=")) {
      person = word.slice("--person=".length);
    } else {
      return { kind: "refused", problem: `unknown argument ${JSON.stringify(word)}` };
    }
  }
  return { kind: "login", device, person };
}

/**
 * Runs `dispatch login` with `argv` (the words after `login`) in `env`, and answers its exit
 * code: 0 once the login is approved and kept; 1 when the broker refused it, the person denied it,
 * its code expired, or it could not be kept; 2 for a usage error, before any request: no
 * `--device`, no person (`--person`, else `AGENT_SECRETS_PERSON`), no broker
 * (`AGENT_SECRETS_URL`) a device login may use (https, or http on this machine's loopback), or a
 * login directory others may write to or another user owns.
 */
export async function loginCommand(
  argv: readonly string[],
  env: Readonly<Record<string, string>>,
  io: LoginIo
): Promise<number> {
  const refuse = (problem: string, code: number): number => {
    io.print(`dispatch login was not called: ${problem}`);
    return code;
  };
  const parsed = parseLogin(argv);
  if (parsed.kind === "help") {
    io.print(LOGIN_HELP);
    return OK;
  }
  if (parsed.kind === "refused")
    return refuse(`${parsed.problem}. Run dispatch login --help.`, USAGE);
  if (!parsed.device) {
    return refuse(
      "it logs a machine in with the broker's device grant alone, so --device is required. Run dispatch login --help.",
      USAGE
    );
  }
  const person = (parsed.person ?? env[PERSON_VAR] ?? "").trim();
  if (!person.includes("@")) {
    return refuse(
      `name the person who approves this machine, by the address they sign in to Dispatch with: --person <address>, or ${PERSON_VAR}.`,
      USAGE
    );
  }
  const brokerUrl = (env[BROKER_URL_VAR] ?? "").trim();
  // Not quoted: a value that is no URL may be a token pasted into the variable.
  if (!isHttpUrl(brokerUrl)) {
    return refuse(
      `${BROKER_URL_VAR} ${brokerUrl === "" ? "names no secrets broker" : "is not an http or https URL"}, so there is no broker to log this machine in at.`,
      USAGE
    );
  }
  let path: string;
  try {
    // An http broker off this machine's loopback is refused here, before any request, and so is
    // a login directory another user could replace the login in: no person approves a login
    // this machine could not keep.
    deviceLoginBroker(brokerUrl);
    path = deviceLoginFile(env);
    await deviceLoginPlace(path);
  } catch (error) {
    return refuse(messageFor(error), USAGE);
  }
  const host = deviceLoginHost(hostname());
  // The login this one replaces, where the machine holds one it can read: its credential stays
  // live at the broker until it lapses or is revoked, so the person is told which to revoke.
  const earlier = await readDeviceLogin(path).then(
    (login) => login.credentialId,
    () => ""
  );
  try {
    const started = await startDeviceLogin({
      brokerUrl,
      person,
      host,
      fetch: io.fetchImpl,
    });
    io.print(
      [
        `dispatch login: to log this machine (${host}) in at the secrets broker at ${urlForText(started.brokerUrl, BROKER_URL_VAR)}, ${person} approves it:`,
        `  open ${started.verificationUriComplete ?? started.verificationUri}`,
        `  and enter the code ${started.userCode}`,
        `dispatch login: the code is good until ${started.expiresAt.toISOString()}; waiting for the approval.`,
      ].join("\n")
    );
    const login = await finishDeviceLogin(started, { path, fetch: io.fetchImpl, sleep: io.sleep });
    const lines = [
      `dispatch login: approved. This machine's device login is kept in ${DEVICE_LOGIN_FILE_NAMED}; its launcher credential ${login.credentialId} lasts until ${login.expiresAt.toISOString()}, and each refresh moves that on.`,
    ];
    if (earlier !== "" && earlier !== login.credentialId) {
      lines.push(
        `dispatch login: the earlier login of this machine, launcher credential ${earlier}, stays live until it lapses or is revoked, and a copy of its file still works; revoke it in Machine logins on ${started.verificationUri}.`
      );
    }
    if ((env[IDENTITY_VAR] ?? "").trim() !== IDENTITY_DEVICE_LOGIN) {
      lines.push(
        `dispatch login: set ${IDENTITY_VAR}=${IDENTITY_DEVICE_LOGIN} in the environment of this machine's sessions, so they trade this login.`
      );
    }
    io.print(lines.join("\n"));
    return OK;
  } catch (error) {
    io.print(`dispatch login: ${messageFor(error)}`);
    return REFUSED;
  }
}
