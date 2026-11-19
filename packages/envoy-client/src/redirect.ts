/**
 * No request that carries a credential follows a redirect. Left to itself, fetch sends the request
 * again to the origin a 3xx's `Location` names, and Bun and Node drop only `Authorization` and
 * `Cookie` on the way: a DPoP proof, an `x-api-key` header, and a 307's or 308's body all reach
 * that origin. So such a request is sent with `redirect: "manual"`, and its caller reads the
 * answer's status before any of its body: a 3xx is the answer, which the caller refuses, saying
 * where it pointed ({@link redirectTarget}, {@link redirectedTo}) and what to correct.
 */

/** Where a refused redirect pointed: another http(s) origin, another path of the origin the
 * request went to, or no target an http(s) request can go to. */
export type RedirectTarget =
  | { readonly kind: "another origin"; readonly origin: string }
  | { readonly kind: "the same origin" }
  | { readonly kind: "no target" };

/**
 * Where `response` redirects, read against `url`, the request it answers; undefined for an answer
 * that is no 3xx, which the caller goes on to read. Another origin is its scheme and host, with a
 * port that is not the scheme's own: never the path, query, fragment or user of the `Location`,
 * any of which can carry a secret. A `Location` that is missing, empty, unreadable or not http(s)
 * is no target. The caller refuses a 3xx and reads none of its body, so this cancels that body: a
 * body that never ends then holds no connection. The Slack poster runs this function's own source
 * in the process that holds the token (`slack-post.ts`), so it names nothing outside its own body.
 */
export function redirectTarget(response: Response, url: string): RedirectTarget | undefined {
  if (response.status < 300 || response.status > 399) return undefined;
  response.body?.cancel().catch(() => {});
  const location = response.headers.get("Location")?.trim() ?? "";
  if (location === "") return { kind: "no target" };
  try {
    const target = new URL(location, url);
    if (target.protocol !== "http:" && target.protocol !== "https:") return { kind: "no target" };
    return target.origin === new URL(url).origin
      ? { kind: "the same origin" }
      : { kind: "another origin", origin: target.origin };
  } catch {
    return { kind: "no target" };
  }
}

/** The longest origin a refusal prints: `https://`, a host name of 253 characters, the most a
 * name holds, and `:65535`. A longer one names no host a person could have meant. */
const LONGEST_SHOWN_ORIGIN = "https://".length + 253 + ":65535".length;

/** The words that say where a refused redirect pointed, for the line that refuses it. */
export function redirectedTo(target: RedirectTarget): string {
  switch (target.kind) {
    case "another origin":
      return target.origin.length > LONGEST_SHOWN_ORIGIN
        ? `redirecting to another address, whose origin is ${target.origin.length} characters long`
        : `redirecting to ${target.origin}`;
    case "the same origin":
      return "redirecting to another path of the same address";
    case "no target":
      return "naming no target";
  }
}

/** Why a server redirects a request to another path of its own address, for the line that says to
 * correct the path of the URL the client was given, which the client puts before its own. */
export const SAME_ADDRESS_CAUSE = 'a missing or an extra "/" is the usual cause';

/** What to check when a server redirects a request to another path of its own address, and the
 * client builds that path itself, whatever path the URL it was given holds: no setting of the
 * client chose the other path, for the line that refuses the redirect. */
export const OWN_PATH_CHECK =
  "the client builds that path itself, so the server at that address, or a proxy in front of it, chose the other one: check what answers there";
