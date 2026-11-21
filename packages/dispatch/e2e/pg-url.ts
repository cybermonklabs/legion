// The split of a Postgres URL that e2e/psql.ts gives psql: the URL without its password as psql's
// argument, and the password for psql's environment (PGPASSWORD). It imports nothing, so
// e2e/psql.ts stays a module any other can import statically. The same rules are the shell's
// packages/envoy/scripts/lib/pg-url.sh and the Python scripts/search-measure/pg.py.

/** A URL `splitDatabaseUrl` will not split. Its message names the fault and nothing of the URL. */
export class DatabaseUrlRefused extends Error {}

/** `url` without its password, and the password decoded (`undefined` when it holds none), split as
 *  libpq splits a URL (fe-connect.c, conninfo_uri_parse_options), so the password taken out is
 *  the one psql would have read:
 *  - the credentials run to the first `@` before the first `/`, and the password is what follows
 *    their first `:`; libpq reads an empty one as none;
 *  - a `password=` in the query string, its name percent-decoded, is a password too: it is taken
 *    out with its `&`, and the last one wins, as in libpq;
 *  - the password is percent-decoded as libpq decodes it, `%XX` being the byte XX;
 *  - every other character of the URL is kept as it is, and a URL with no password is kept whole.
 *  It throws `DatabaseUrlRefused` for what libpq refuses in a password (a `%` that starts no `%XX`
 *  escape, and `%00`), for a password whose bytes are not UTF-8 (no environment value can carry
 *  them), for a host in `[ ]` libpq cannot read, for a raw `@` in the hosts (the text past a
 *  password's raw `@`, which libpq would print in its host error), and for anything but a
 *  `postgres://` or `postgresql://` URL, since a key=value string can hold a password this does
 *  not take out. */
export function splitDatabaseUrl(url: string): { url: string; password: string | undefined } {
  const scheme = /^postgres(ql)?:\/\//.exec(url)?.[0];
  if (scheme === undefined) refuse("it is not a postgres:// or postgresql:// URL");
  const rest = url.slice(scheme.length);
  let password: string | undefined;
  let user = "";
  let location = rest;
  if (rest.split("/", 1)[0].includes("@")) {
    const at = rest.indexOf("@");
    const credentials = rest.slice(0, at);
    location = rest.slice(at + 1);
    user = `${credentials}@`;
    const colon = credentials.indexOf(":");
    if (colon !== -1 && colon < credentials.length - 1) {
      password = decode(credentials.slice(colon + 1), "its password");
      user = `${credentials.slice(0, colon)}@`;
    }
  }
  // libpq takes the credentials to the first `@` and reads a second one as part of a host, whose
  // error then prints it: a password's `@` that is not written `%40` leaves its tail there.
  if (location.split(/[/?]/, 1)[0].includes("@")) refuse("a host holds a raw @");
  // The query string starts at the first `?` past the hosts, read as libpq reads them: a host in
  // `[ ]` runs to its `]`, every other host and each port to a `:`, `/`, `?` or the `,` before the
  // next host, and the database to a `?`. `tail` is what is left past each step.
  let tail = location;
  for (;;) {
    if (tail.startsWith("[")) {
      const close = tail.indexOf("]");
      if (close === -1) refuse("a host's [ has no ]");
      tail = tail.slice(close + 1);
    } else {
      tail = tail.replace(/^[^:/?,]*/, "");
    }
    if (tail.startsWith(":")) tail = tail.slice(1).replace(/^[^/?,]*/, "");
    if (!tail.startsWith(",")) break;
    tail = tail.slice(1);
  }
  if (tail !== "" && !tail.startsWith("/") && !tail.startsWith("?")) {
    refuse("a host's ] is followed by something other than :, /, ? or ,");
  }
  if (tail.startsWith("/")) tail = tail.includes("?") ? tail.slice(tail.indexOf("?")) : "";
  const head = location.slice(0, location.length - tail.length);
  const kept: string[] = [];
  let removed = false;
  for (const param of tail === "" ? [] : tail.slice(1).split("&")) {
    const equals = param.indexOf("=");
    const key = equals === -1 ? "" : param.slice(0, equals);
    if (percentDecoded(key) === "password") {
      password = decode(param.slice(equals + 1), "its query string's password");
      removed = true;
    } else {
      kept.push(param);
    }
  }
  if (removed) tail = kept.length === 0 ? "" : `?${kept.join("&")}`;
  return { url: `${scheme}${user}${head}${tail}`, password };
}

/** A `%` that starts no `%XX` escape, or `%00`: libpq refuses both. */
const malformed = /%(?![0-9A-Fa-f]{2})|%00/;

/** `text` percent-decoded as libpq decodes it, which must be UTF-8 to reach an environment. */
function decode(text: string, what: string): string {
  if (malformed.test(text)) refuse(`${what} holds a % that starts no %XX escape, or %00`);
  const decoded = percentDecoded(text);
  if (decoded === undefined) refuse(`${what} decodes to bytes that are not UTF-8`);
  return decoded;
}

/** `text` with each `%XX` the byte XX, read as UTF-8 with every byte kept (a leading byte-order
 *  mark too, as libpq keeps it), or `undefined` for a `%` that starts no escape or bytes that are
 *  not UTF-8. */
function percentDecoded(text: string): string | undefined {
  try {
    return decodeURIComponent(text);
  } catch {
    return undefined;
  }
}

function refuse(fault: string): never {
  throw new DatabaseUrlRefused(`refused a Postgres URL: ${fault} (the URL is not printed)`);
}
