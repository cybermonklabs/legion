// What a text a log may print says of a configured value. Every client of this workspace names a
// configured URL, and the error a library or the runtime threw over one, through these functions:
// the Dispatch client and its commands, the Envoy client and its listener calls, Claude Code's
// session hook, and the broker client's trades. One rule, so no client names more of a setting
// than another, and the rule of the Go clients (`packages/envoy/internal/redact`):
//  - a configured URL prints only when it is provably a plain http(s) URL with no place a
//    credential can sit (`isCleanUrl`); a Postgres URL prints as its scheme, host, port and
//    database alone, where its host passes the same host rule; any other value, one that does not
//    parse as a URL included, prints as a fixed phrase naming its setting, and no part of it is
//    ever printed;
//  - a request URL a client built from such a URL prints only when its base is clean, and then
//    with the path and query the client wrote (`requestForText`);
//  - an error a library or the runtime threw is named by its class alone (`errorClass`), since
//    its text can repeat the value it was handed;
//  - the error a text leaves out is not kept as its cause, or it is printed with the text.
//
// No part of an http(s) URL that fails the check is printed, not even with its user info taken
// out: the URL parser can split it otherwise than its writer meant. `https://user:p@ssw0rd` names
// no host, and the parser reads `ssw0rd` as one; `https://tok?en@broker.example` is a host `tok`
// and a query. Either would print part of a credential. A Postgres URL is the one form printed
// with its user info taken out (`postgresForText`), and only where it reads one way to every
// parser.

/** What stands in for a URL `urlForText` and `requestForText` will not print, after its setting. */
export const URL_NOT_PRINTED =
  "(not printed: it carries a credential or is not a plain http(s) URL)";

/**
 * Whether `raw` is a plain http(s) URL that names no place a credential can sit, as Go's
 * `redact.Clean` reads one. All of these hold:
 *  - every character is printable ASCII other than `@`, `%`, `?`, `#` and `\` (so no user info,
 *    no escape, no query, no fragment, no whitespace and no control character);
 *  - it reads as an http or https scheme (any case), `://`, a host and a path, and the URL parser
 *    takes it;
 *  - the host is a DNS name (an IPv4 address among them) or a bracketed IPv6 address with no
 *    zone, with an optional port of one to five digits;
 *  - the path is empty or only letters, digits, `.`, `_`, `~`, `/` and `-`.
 */
export function isCleanUrl(raw: string): boolean {
  for (let at = 0; at < raw.length; at += 1) {
    const code = raw.charCodeAt(at);
    if (code <= 0x20 || code >= 0x7f || `@%?#\\`.includes(raw[at] ?? "")) return false;
  }
  const scheme = /^(https?):\/\//i.exec(raw);
  if (scheme === null || !URL.canParse(raw)) return false;
  const rest = raw.slice(scheme[0].length);
  const slash = rest.indexOf("/");
  const hostPort = slash < 0 ? rest : rest.slice(0, slash);
  const path = slash < 0 ? "" : rest.slice(slash);
  return isHostPort(hostPort) && /^[A-Za-z0-9._~/-]*$/.test(path);
}

/** Whether `hostPort` is a DNS name or a bracketed IPv6 address with no zone, with an optional
 *  port of one to five digits (Go's `redact.hostPort`). */
function isHostPort(hostPort: string): boolean {
  let host = hostPort;
  let port: string | undefined;
  if (hostPort.startsWith("[")) {
    const end = hostPort.indexOf("]");
    if (end < 0) return false;
    host = hostPort.slice(1, end);
    const after = hostPort.slice(end + 1);
    if (after !== "") {
      if (!after.startsWith(":")) return false;
      port = after.slice(1);
    }
    if (!/^[0-9A-Fa-f:.]+$/.test(host) || !URL.canParse(`http://[${host}]`)) return false;
  } else {
    const colon = hostPort.indexOf(":");
    if (colon >= 0) {
      host = hostPort.slice(0, colon);
      port = hostPort.slice(colon + 1);
    }
    if (!isDnsName(host)) return false;
  }
  return port === undefined || /^[0-9]{1,5}$/.test(port);
}

/** Whether `name` is dot-separated labels of 1 to 63 letters, digits and `-`, none starting or
 *  ending with `-`, 253 characters at most (Go's `redact.dnsName`). */
function isDnsName(name: string): boolean {
  if (name === "" || name.length > 253) return false;
  return name
    .split(".")
    .every((label) => /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(label));
}

/**
 * `url`, a configured URL, as a text names it: an error, a log line, a notice. It is `url` as
 * written where `isCleanUrl` passes it; a Postgres URL as `postgresForText` names it; and otherwise
 * `<setting> (not printed: …)`, `setting` being the setting `url` was read from or what names it.
 * Requests still go to the URL as configured.
 */
export function urlForText(url: string, setting: string): string {
  if (isCleanUrl(url)) return url;
  return postgresForText(url) ?? `${setting} ${URL_NOT_PRINTED}`;
}

/**
 * A `postgres://` or `postgresql://` URL as `<scheme>//<host>[:port]/<database>`, with no user, no
 * password and no query, or undefined. It is answered only where the value reads one way to every
 * parser: printable ASCII with no `\`, the scheme in lower case (pgx reads any other case as
 * `key=value` text), at most one `@` and that one before the first `/`, `?` or `#`, a host that
 * passes `isHostPort`, a database of letters, digits, `.`, `_`, `~` and `-`, and a query with no
 * `@` and no fragment. A password holding a raw `/`, `?` or `#`, or a second `@` (Go's parser
 * splits at the last `@` and libpq's at the first), fails one of these, so no part of it is
 * printed. A password whose raw `@` is its only one reads to both parsers as a password's head
 * and a host, and that host is printed: it is the host the client dials.
 */
function postgresForText(url: string): string | undefined {
  for (let at = 0; at < url.length; at += 1) {
    const code = url.charCodeAt(at);
    if (code <= 0x20 || code >= 0x7f || url[at] === "\\") return undefined;
  }
  const parts =
    /^(postgres(?:ql)?:)\/\/(?:[^@/?#]*@)?([^@/?#]*)(\/[A-Za-z0-9._~-]*)?(?:\?[^@#]*)?$/.exec(url);
  const [, scheme, hostPort, database = ""] = parts ?? [];
  if (scheme === undefined || hostPort === undefined || !isHostPort(hostPort)) return undefined;
  return `${scheme}//${hostPort}${database}`;
}

/**
 * `requestUrl`, a request URL a client built from the configured `base`, its own path and its own
 * query, as a text names it: as built where `base` is clean and `requestUrl` starts with it, the
 * scheme and host in any case, since the URL parser lowers both (`?project=DSP&limit=2` names the
 * request, and the client wrote it from its own arguments); otherwise as `urlForText` names
 * `base`. A request URL that does not start with its base is on no URL this rule checked.
 */
export function requestForText(base: string, requestUrl: string, setting: string): string {
  if (!isCleanUrl(base)) return `${setting} ${URL_NOT_PRINTED}`;
  const head = requestUrl.slice(0, base.length);
  const rest = requestUrl.slice(base.length);
  // What follows the base starts its path or its query, so `http://host` does not name
  // `http://hostile.example`.
  const follows = rest === "" || base.endsWith("/") || rest.startsWith("/") || rest.startsWith("?");
  if (head.toLowerCase() !== base.toLowerCase() || !follows) return `${setting} ${URL_NOT_PRINTED}`;
  return `${base}${rest}`;
}

/** Whether `value` is an http or https URL, as a client checks a configured server address before
 *  it sends to it. A URL that passes may still not print (`isCleanUrl`). */
export function isHttpUrl(value: string): boolean {
  if (!URL.canParse(value)) return false;
  const { protocol } = new URL(value);
  return protocol === "http:" || protocol === "https:";
}

/** A code or a syscall as a runtime or a library sets them: one word (`EAI_AGAIN`,
 *  `ERR_SOCKET_BAD_PORT`, `OAUTH_RESPONSE_IS_NOT_CONFORM`), never text taken from a setting. */
const ERROR_CODE = /^[A-Za-z][A-Za-z0-9_]{0,63}$/;
const SYSCALL = /^[a-z]+$/;
const ERROR_NAME = /^[A-Za-z]+$/;

/**
 * What a text says of an error a library or the runtime threw: its class, never its message,
 * which repeats what it was handed (a resolver's error names the host, a URL parser's the URL, a
 * file read's the path). The class is its syscall and code where it has a code
 * (`getaddrinfo EAI_AGAIN`, `ECONNREFUSED`, `OAUTH_INVALID_SERVER_METADATA`), else its type and
 * `(message withheld)`.
 */
export function errorClass(error: unknown): string {
  if (!(error instanceof Error)) return `a thrown ${typeof error} (message withheld)`;
  const { code, syscall } = error as { code?: unknown; syscall?: unknown };
  if (typeof code === "string" && ERROR_CODE.test(code))
    return typeof syscall === "string" && SYSCALL.test(syscall) ? `${syscall} ${code}` : code;
  return `${ERROR_NAME.test(error.name) ? error.name : "Error"} (message withheld)`;
}
