// packages/envoy-client/src/dispatch-commands/login.ts
//
// `dispatch login --device`: logs this machine in at the secrets broker's device grant, once per
// machine, for a machine with no platform identity of its own, such as a person's laptop. It asks
// the broker for a code, prints where to type it and the code, waits while the person approves
// it in Dispatch, and keeps the login in its file (`@legion/cipher-client/device-login`). After
// that, every session of the machine that sets `AGENT_SECRETS_MACHINE_IDENTITY=device-login`
// trades the login for its broker token.
//
// It is no Dispatch tool: a person runs it in a terminal, before any session exists, so it reads
// no host, no session and no envoy.json, only the environment's broker settings. It refuses while
// either of the command's token files is set, after its own `--help`; `tokenFilesTarget` in
// `dispatch-config.ts` says when a Dispatch call runs on the files. Its name, its help and the
// sentence `dispatch --help` lists for it are `dispatch-login-command.ts`'s.

import { hostname } from "node:os";
import {
  type DeviceAuthorization,
  deviceLoginBroker,
  deviceLoginHost,
  finishDeviceLogin,
  startDeviceLogin,
} from "@legion/cipher-client/device-login";
import {
  DEVICE_LOGIN_FILE_NAMED,
  deviceLoginFile,
  deviceLoginPlace,
} from "@legion/cipher-client/device-login-file";
import {
  BROKER_URL_VAR,
  IDENTITY_DEVICE_LOGIN,
  IDENTITY_VAR,
} from "@legion/cipher-client/environment";
import { PERSON_VAR } from "@legion/cipher-client/signins";
import { isHttpUrl, urlForText } from "@legion/contracts/log-text";
import { settingsBesideTokenFiles, tokenFilesSet } from "../dispatch-config";
import { loginHelp } from "../dispatch-login-command";
import { messageFor } from "../errors";
import { OK, REFUSED, USAGE } from "./exit-codes";

/** What the command writes to and waits with. */
export interface LoginIo {
  /** Writes one line or more of what the person reads. */
  print(text: string): void;
  readonly fetchImpl?: typeof fetch | undefined;
  /** Waits between two polls of the code; a timer unless a test replaces it. */
  readonly sleep?: ((ms: number) => Promise<void>) | undefined;
}

/** The flags `argv` sets, or the problem with them. `--scope` is read for its syntax alone:
 *  `none`, or comma-separated words, each trimmed; `startDeviceLogin` holds each word, and the
 *  reason's length, to what a Dispatch sign-in may ask. */
type ParsedLogin =
  | { readonly kind: "help" }
  | {
      readonly kind: "login";
      readonly device: boolean;
      readonly dispatchScopes: readonly string[] | undefined;
      readonly person: string | undefined;
      readonly reason: string | undefined;
    }
  | { readonly kind: "refused"; readonly problem: string };

function parseLogin(argv: readonly string[]): ParsedLogin {
  let device = false;
  let person: string | undefined;
  let scope: string | undefined;
  let reason: string | undefined;
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
    } else if (word === "--scope") {
      const next = argv[at + 1];
      if (next === undefined) return { kind: "refused", problem: "--scope needs a list" };
      scope = next;
      at += 1;
    } else if (word.startsWith("--scope=")) {
      scope = word.slice("--scope=".length);
    } else if (word === "--reason") {
      const next = argv[at + 1];
      if (next === undefined) return { kind: "refused", problem: "--reason needs text" };
      reason = next;
      at += 1;
    } else if (word.startsWith("--reason=")) {
      reason = word.slice("--reason=".length);
    } else {
      return { kind: "refused", problem: `unknown argument ${JSON.stringify(word)}` };
    }
  }
  if (scope === undefined) {
    return { kind: "login", device, dispatchScopes: undefined, person, reason };
  }
  const dispatchScopes = scope.trim() === "none" ? [] : scope.split(",").map((word) => word.trim());
  return { kind: "login", device, dispatchScopes, person, reason };
}

/**
 * Runs `dispatch login` with `argv` (the words after `login`) in `env`, and answers its exit
 * code: 0 once the login is approved and kept; 1 when the broker could not be reached or refused
 * it, answered a code that would lapse before its first poll (refused before the code is printed,
 * naming the broker's interval and its codes' lifetime, or the hour this machine polls a code at
 * most, and what must change at the broker), or answered a poll with a 4xx that is no outage and
 * decides nothing (naming its status), when the person denied it, its code expired, or it could
 * not be kept (the message then names its credential, live at the broker, and any copy of it
 * kept); 2 for a usage error, before any request: either of the command's token files set
 * (`tokenFilesSet`), no `--device`, no person (`--person`, else `AGENT_SECRETS_PERSON`), no broker
 * (`AGENT_SECRETS_URL`) a device login may use (https, or http on this machine's loopback), a
 * login directory others may write to, another user owns, or this user cannot enter and write to,
 * a login file setting that names something other than a regular file (a directory, a named
 * pipe, a socket, a device, a loop of links), or a `--scope` word no Dispatch sign-in may ask for
 * or a `--reason` longer than the broker takes, which `startDeviceLogin` refuses with a
 * `RangeError` before it asks.
 *
 * A Dispatch sign-in (`--scope`) names itself as one, with its `--reason` where one is given, and
 * never as this machine's login at the broker, which would read as a machine login's sessions and
 * secrets: each line names the login as `startDeviceLogin` describes it.
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
    io.print(loginHelp());
    return OK;
  }
  // The token files are the dispatch command's only Dispatch credential, which trades nothing at
  // the broker (`tokenFilesTarget`): a device login beside them would ask the broker and keep a
  // login on the copy. It is refused before either, naming each setting set beside them as every
  // Dispatch call names it.
  const tokenFiles = tokenFilesSet(env);
  if (tokenFiles !== undefined) {
    const beside = settingsBesideTokenFiles(env);
    return refuse(
      `${tokenFiles}, and the token files are the dispatch command's only Dispatch credential, which trades nothing at the secrets broker; a device login asks the broker and keeps a login in a file, so it does not run beside them${beside.length === 0 ? "" : `: ${beside.join("; ")}`}. Run dispatch login in a shell that sets neither token file.`,
      USAGE
    );
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
    // a login directory another user could replace the login in, one this user cannot enter and
    // write to, and a file setting that names anything but a regular file: no person approves a
    // login this machine could not keep.
    deviceLoginBroker(brokerUrl);
    path = deviceLoginFile(env);
    await deviceLoginPlace(path);
  } catch (error) {
    return refuse(messageFor(error), USAGE);
  }
  const host = deviceLoginHost(hostname());
  let started: DeviceAuthorization;
  try {
    started = await startDeviceLogin({
      brokerUrl,
      dispatchScopes: parsed.dispatchScopes,
      person,
      host,
      reason: parsed.reason,
      fetch: io.fetchImpl,
    });
  } catch (error) {
    // What a Dispatch sign-in may not ask for is refused before any request, as a RangeError.
    if (error instanceof RangeError)
      return refuse(`${error.message}. Run dispatch login --help.`, USAGE);
    io.print(`dispatch login: ${messageFor(error)}`);
    return REFUSED;
  }
  try {
    const naming =
      parsed.reason === undefined ? host : `${JSON.stringify(parsed.reason)} on ${host}`;
    io.print(
      [
        `dispatch login: for ${person} to approve ${started.describe} (${naming}) through the secrets broker at ${urlForText(started.brokerUrl, BROKER_URL_VAR)}:`,
        `  open ${started.verificationUriComplete ?? started.verificationUri}`,
        `  and enter the code ${started.userCode}`,
        `dispatch login: the code is good until ${started.expiresAt.toISOString()}; waiting for the approval.`,
      ].join("\n")
    );
    // The login it replaced is read under the lock its save holds: its credential stays live at
    // the broker until it lapses or is revoked, so the person is told which to revoke.
    const login = await finishDeviceLogin(started, { path, fetch: io.fetchImpl, sleep: io.sleep });
    // A machine login's line is byte for byte the one earlier builds print, which the packed
    // terminal tests read; a sign-in's names it in the same words (`started.describe`).
    const lines = [
      `dispatch login: approved. ${started.describe.charAt(0).toUpperCase()}${started.describe.slice(1)} is kept in ${DEVICE_LOGIN_FILE_NAMED}; its launcher credential ${login.credentialId} lasts until ${login.expiresAt.toISOString()}, and each refresh moves that on.`,
    ];
    if (login.dispatchScopes !== null) {
      lines.push(
        `dispatch login: approved Dispatch scopes: ${login.dispatchScopes.join(", ") || "none"}.`
      );
    }
    if (login.lockLeft !== "") {
      // The login is kept and in use, so this names no credential to revoke.
      lines.push(`dispatch login: the login is kept, but ${login.lockLeft}.`);
    }
    if (login.replaced !== "" && login.replaced !== login.credentialId) {
      lines.push(
        `dispatch login: the earlier login of this machine, launcher credential ${login.replaced}, stays live until it lapses or is revoked, and a copy of its file still works; revoke it in Machine logins on ${started.verificationUri}.`
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
