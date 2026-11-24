// packages/contracts/src/url-join.ts
//
// A request URL built from a configured URL's text and a path: the path joined onto the configured
// URL's path, the configured query kept ahead of the path's own, user information, host and port
// kept, and the fragment, which no request sends, dropped. A path appended to a configured URL's
// text sits inside its query or its fragment where it has one (`http://host/api?key=v` with
// `/v1/x` appended is `http://host/api?key=v/v1/x`), so the request goes to another path and the
// query's value changes. It is the TypeScript side of the Go modules' `urljoin.Join`, with the same
// rules, and it sits in this package so envoy-client's and cipher-client's requests join through
// one copy.

/** What `joinUrl` throws for a base that does not parse or has no host. It quotes no part of the
 *  base: the `TypeError` the URL parser throws carries the text it refused (`input`), user
 *  information and query included, so it is neither thrown nor kept as a cause. */
const BASE_REFUSED = "joinUrl: the URL is not an absolute URL with a host (it is not printed)";

/** What `joinUrl` throws for a ref that is not a relative reference, or holds a scheme, a host or a
 *  fragment (`refusedRef`). */
const REF_REFUSED =
  "joinUrl: the path to join is not a relative reference without a host or a fragment (it is not printed)";

/**
 * `base`, a configured URL's text, with `ref` joined onto it. `ref` is a path, with or without a
 * leading slash, and optionally its own query (`/v1/x`, `v1/x`, `/v1/x?a=b`); it never names a
 * scheme or a host. The result is `base` as the URL parser reads it, with:
 * - its path joined with `ref`'s as Go's `url.URL.JoinPath` joins them: a leading slash in `ref`
 *   does not replace `base`'s path, the joined path is cleaned of `.` and `..` segments and
 *   repeated slashes, and `ref`'s trailing slash is kept. `ref`'s path is read as already escaped,
 *   so a segment `encodeURIComponent` wrote (an escaped `/` as `%2F`) reaches the request as
 *   written; a `ref` with no path leaves `base`'s path as it is;
 * - `base`'s query first and `ref`'s after, joined by `&`, neither decoded or encoded again;
 * - `base`'s user information, host and port;
 * - no fragment.
 *
 * It throws an `Error` in fixed words, quoting neither text, for a `base` that does not parse or
 * has no host, and for a `ref` that `refusedRef` refuses.
 */
export function joinUrl(base: string, ref: string): URL {
  let url: URL;
  try {
    url = new URL(base);
  } catch {
    throw new Error(BASE_REFUSED);
  }
  if (url.host === "") throw new Error(BASE_REFUSED);
  const queryAt = ref.indexOf("?");
  const refPath = queryAt < 0 ? ref : ref.slice(0, queryAt);
  const refQuery = queryAt < 0 ? "" : ref.slice(queryAt + 1);
  if (refusedRef(ref, refPath)) throw new Error(REF_REFUSED);
  if (refPath !== "") url.pathname = joinPaths(url.pathname, refPath);
  if (refQuery !== "") url.search = url.search === "" ? refQuery : `${url.search}&${refQuery}`;
  url.hash = "";
  return url;
}

/** Whether `ref`, whose path is `refPath`, is one Go's `url.Parse` refuses as a relative reference,
 *  or reads as holding a scheme, a host or a fragment, as `urljoin.Join` refuses it: it begins
 *  with `//` (a host), holds a `#` (a fragment), a colon in its first segment (a scheme, or a
 *  segment a relative reference may not hold), a `%` in its path that starts no escape, or a
 *  control byte. A backslash is refused too, which the URL parser reads as a slash in an http(s)
 *  URL, and Go's as a character of the path. */
function refusedRef(ref: string, refPath: string): boolean {
  if (ref.startsWith("//") || /[#\\]/.test(ref) || /^[^/]*:/.test(refPath)) return true;
  if (/%(?![0-9A-Fa-f]{2})/.test(refPath)) return true;
  for (let at = 0; at < ref.length; at++) {
    const code = ref.charCodeAt(at);
    if (code < 0x20 || code === 0x7f) return true;
  }
  return false;
}

/** `base` and `ref`, two escaped paths, joined as Go's `path.Join` joins them, rooted at `/`, with
 *  `ref`'s trailing slash kept. */
function joinPaths(base: string, ref: string): string {
  const segments: string[] = [];
  for (const segment of `${base}/${ref}`.split("/")) {
    if (segment === "" || segment === ".") continue;
    if (segment === "..") segments.pop();
    else segments.push(segment);
  }
  const joined = `/${segments.join("/")}`;
  return ref.endsWith("/") && joined !== "/" ? `${joined}/` : joined;
}
