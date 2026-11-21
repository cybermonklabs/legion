import { scrub } from "@legion/contracts/held-secrets";
import { errorClass } from "@legion/contracts/log-text";

/**
 * The code of the refusal `error` carries, or undefined for an error carrying none.
 *
 * A refusal Dispatch decided (`DispatchServiceError`, and the gateway answer that stands in for
 * one) carries the code of the first rule the call broke beside its message, and `fromDispatch`
 * is what tells such an error from a Node errno error, which carries a `code` of its own (ENOENT,
 * EPERM, …) that says nothing about what Dispatch decided. The check is structural rather than an
 * `instanceof`, because those classes live in `dispatch-http.ts`, which reads this module's
 * neighbours.
 */
function dispatchCode(error: Error): string | undefined {
  if (!("fromDispatch" in error) || !("code" in error)) return undefined;
  const code = error.code;
  return typeof code === "string" && code !== "" ? code : undefined;
}

/**
 * Coerce an unknown thrown value into a human-readable message.
 *
 * A refusal Dispatch decided reads `<CODE>: <message>`, because `skill://dispatch` has every agent
 * branch on an error's code rather than on its words, and the code was in no line the command
 * printed: a reader had to run the same call through curl to see it (reported by the tree of
 * dispatch://INFRA-1523). A message that already opens with its own code is left as it is, and
 * every other thrown value prints as it always did. Every text it answers passes the scrub of the
 * secrets this process holds (`@legion/contracts/held-secrets`): a server at the wrong URL can
 * echo the bearer it was sent inside a refusal this prints as guidance.
 */
export function messageFor(error: unknown): string {
  if (!(error instanceof Error)) return scrub(String(error));
  const code = dispatchCode(error);
  return scrub(
    code === undefined || error.message.startsWith(code)
      ? error.message
      : `${code}: ${error.message}`
  );
}

/** True when `error` is a Node errno error carrying `code` (ENOENT, EPERM, …). */
export function hasErrnoCode(error: unknown, code: string): boolean {
  return error instanceof Error && "code" in error && error.code === code;
}

/**
 * The `name` of consumer delivery's refusal of a session that holds no relay subject
 * (`NoRelaySubjectError`, `./consumer-delivery`). Each plugin bundles its own copy of this package,
 * so the refusal pi-envoy throws is no `instanceof` of the class in pi-legion's copy: a caller that
 * hears it across that line reads its name (`isNoRelaySubjectRefusal`).
 */
export const NO_RELAY_SUBJECT_ERROR_NAME = "NoRelaySubjectError";

/**
 * Whether `error` is consumer delivery's refusal of a session that holds no relay subject, in
 * whichever plugin's copy of this package it was made, or carries one as its `cause`: pi-envoy's
 * session pass wraps the refusal in its own registration error (`cause`), and its registration
 * call throws it bare, so a role claim can fail in either shape. At most 8 causes are followed.
 */
export function isNoRelaySubjectRefusal(error: unknown): boolean {
  let current: unknown = error;
  for (let depth = 0; depth < 8 && current instanceof Error; depth += 1) {
    if (current.name === NO_RELAY_SUBJECT_ERROR_NAME) return true;
    current = current.cause;
  }
  return false;
}

/** The codes a fetch fails with when nothing took its connection: Bun 1.4 answers a refused
 *  connection `ECONNREFUSED`, and a name that does not resolve `ENOTFOUND` or `EAI_AGAIN` with the
 *  syscall `getaddrinfo` (measured on Bun 1.4.4). A reset after the request was written
 *  (`ECONNRESET`) or a timeout is none of these. */
const NOT_CONNECTED = new Set(["ECONNREFUSED", "EHOSTUNREACH", "ENETUNREACH"]);

/**
 * Whether a fetch's failure says its request reached no server at all: nothing took the
 * connection, or the name did not resolve. Such a request did nothing, so a write it carried cannot
 * have landed; any other failure of a request that went out (a reset after the server read it)
 * may have followed a write the server ran. The Envoy client (`transport.ts`) and the Dispatch
 * client (`dispatch-http.ts`) both read a failure through this.
 */
export function reachedNothing(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  const { code, syscall } = error as { code?: unknown; syscall?: unknown };
  return syscall === "getaddrinfo" || (typeof code === "string" && NOT_CONNECTED.has(code));
}

/**
 * An error whose message its writer composed from nothing a NATS server setting holds, so a log
 * prints it whole (`natsFailureReason`, below), where it prints only the code of nats.js's and
 * the runtime's own errors: this package's own reasons (a hub with no auth service, a trade
 * unanswered, a switch not followed) and a caller's. It takes no cause: a cause is printed with
 * the error wherever the error is printed whole (`console.warn(text, error)`, an uncaught error),
 * and the error it would hold is the one whose text the message leaves out (a token answer, a URL
 * with its user info). Both live here, beside `messageFor`, since `./nats-credential`, which every
 * bundle reading `envoySettings` imports, uses them and imports nats.js's types alone.
 */
export class LoggableError extends Error {
  constructor(message: string) {
    super(message);
  }
}

/** The refusals nats-server sends in its own fixed words (server/errors.go), which nats.js keeps
 *  as an `-ERR`'s whole text (protocol.js `toError`). A text that equals one of them, quotes
 *  aside, is printed as written here; any other text a server sent, under any code, may be a
 *  peer's echo of what the client sent it, its token included, and is withheld. */
const SERVER_REFUSALS: readonly string[] = [
  "Authorization Violation",
  "User Authentication Expired",
  "Account Authentication Expired",
  "Authentication Timeout",
];
/** The codes nats.js gives an `-ERR` the server itself sent (core.js `ErrorCode`). */
const SERVER_ERROR_CODES: Record<string, true> = {
  AUTHORIZATION_VIOLATION: true,
  AUTHENTICATION_EXPIRED: true,
  AUTHENTICATION_TIMEOUT: true,
  ACCOUNT_EXPIRED: true,
  PERMISSIONS_VIOLATION: true,
  NATS_PROTOCOL_ERR: true,
};

/** What a log says of a server's `-ERR` text: nats-server's own fixed words where the text is
 *  one of them, and otherwise that the text is withheld. */
function serverText(code: string, text: string): string {
  const said = text
    .trim()
    .replace(/^'(.*)'$/s, "$1")
    .trim()
    .toLowerCase();
  const refusal = SERVER_REFUSALS.find((fixed) => fixed.toLowerCase() === said);
  if (refusal !== undefined) return refusal;
  return code === "PERMISSIONS_VIOLATION"
    ? "the server refused a permission (its text is withheld)"
    : "the server refused the connection (its text is withheld)";
}

/**
 * Why a NATS call failed, as a log may print it. An allowlist: nothing nats.js, the runtime or a
 * peer derives from the configured servers, or from what the client sent, reaches it. nats.js
 * passes the runtime's own errors on unchanged, and those quote what they were handed (a
 * resolver's error names the host it looked up, a socket's the port, a URL parser's the whole
 * URL), and it keeps a server's `-ERR` text whole, which a peer that is no nats-server can fill
 * with the CONNECT it was sent:
 *  - a `LoggableError` (this package's or a caller's own message): its message;
 *  - nats.js's error for a server's `-ERR`: nats-server's fixed words for its refusal
 *    (`Authorization Violation`, `User Authentication Expired`, …), else that the text is
 *    withheld;
 *  - any other nats.js error: its code (`CONNECTION_REFUSED`, `TIMEOUT`, `TLS`, …);
 *  - anything else: its class (`errorClass`): a runtime error's syscall and code
 *    (`getaddrinfo EAI_AGAIN`, `ERR_SOCKET_BAD_PORT`), else its type, `(message withheld)`.
 */
export function natsFailureReason(error: unknown): string {
  if (error instanceof LoggableError) return scrub(error.message);
  if (error instanceof Error && error.name === "NatsError") {
    const { code } = error as { code?: unknown };
    if (typeof code === "string" && code !== "")
      return SERVER_ERROR_CODES[code] === true ? serverText(code, error.message) : code;
  }
  return errorClass(error);
}
