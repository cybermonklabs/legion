/** What `e2e/acceptance/replace-blocks.sh` writes to stderr when it refuses a connection string: never
 *  the password, whatever characters the password holds and whether or not the driver could read
 *  the string at all.
 *
 *  The driver refuses a DATABASE_URL whose database it cannot read, or whose path or query holds a
 *  percent sign, before it starts anything, and each refusal names the string it refused through
 *  `masked_dsn`, since stderr reaches whatever log keeps it. At legion/INFRA-1520 214b80d40681 that
 *  masker read a URL's authority up to the first `/` or `?` after `//`, so a raw password holding
 *  either character ended the authority inside the password, no `@` was found, and the refusal
 *  printed the string whole: a URL whose password was `abc/def+ghi` put `abc/def+ghi` on stderr.
 *  A generated password is often base64, which holds `/`.
 *
 *  Two rules hold the guard. A password a masker can find is masked, whichever of `/`, `?` or `@`
 *  it holds. And a refusal never prints a connection string it could not mask: a string the masker
 *  cannot parse is named by the reason for its refusal alone, so the next spelling nobody thought
 *  of fails closed rather than to a log. The rows that held before stay as controls of the first
 *  rule.
 *
 *  No row starts a Dispatch: every string is refused before the instance would start.
 */

import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** One spawn of the driver, which refuses before it starts anything. */
const refusalTimeout = 30_000;

/** The driver run from the package root with `databaseUrl` and no other Dispatch, Envoy or libpq
 *  variable, so the refusal is the guard's own: its exit status and what it wrote to stderr. */
async function refusing(databaseUrl: string): Promise<{ code: number; stderr: string }> {
  const child = Bun.spawn(["bash", "e2e/acceptance/replace-blocks.sh"], {
    cwd: dispatchPackage,
    env: { DATABASE_URL: databaseUrl, HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "" },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
  return { code, stderr };
}

/** A postgres:// URL whose userinfo carries `password`. The URL is built here rather than written
 *  whole, so the source holds no `user:password@host` literal for a credential scanner to stop on:
 *  every password in this file is made up for the row that names it. */
function withPassword(password: string, rest: string): string {
  return `postgres://legion_test:${password}@127.0.0.1:5433/${rest}`;
}

for (const { shape, databaseUrl, secret } of [
  {
    shape: "a password of letters, refused for a percent sign in its query",
    databaseUrl: withPassword("sekrit", "?db%6Eame=legion_test"),
    secret: "sekrit",
  },
  {
    shape: "the keyword form",
    databaseUrl: "host=127.0.0.1 port=5433 password=sekrit dbname=mine",
    secret: "sekrit",
  },
  {
    shape: "the keyword form with a single-quoted password",
    databaseUrl: "host=127.0.0.1 password='se kr it' dbname=mine",
    secret: "se kr it",
  },
  {
    shape: "a password holding a slash, as a base64 one does",
    databaseUrl: withPassword("abc/def+ghi", "mine"),
    secret: "abc/def+ghi",
  },
  {
    shape: "a password holding a question mark",
    databaseUrl: withPassword("abc?def", "mine"),
    secret: "abc?def",
  },
  {
    shape: "a password holding a raw at sign",
    databaseUrl: withPassword("ab@cd", "?db%6Eame=mine"),
    secret: "ab@cd",
  },
]) {
  test(
    `a refusal writes no password to stderr: ${shape}`,
    async () => {
      const { code, stderr } = await refusing(databaseUrl);
      expect(
        { code, printed: stderr.includes(secret) },
        "The refusal wrote the password to stderr. Find a URL's userinfo through the last `@` " +
          "before the host, not through the first `/` or `?` after `//`, which a raw password " +
          "can hold."
      ).toEqual({ code: 2, printed: false });
    },
    refusalTimeout
  );
}

test(
  "a refusal of a string the masker cannot parse names the reason and none of the string",
  async () => {
    // A person who meant user:password and left the host out: no `@` for a masker to find, no
    // database for the guard to read, so the guard refuses it and the masker cannot tell where a
    // password would stand.
    const afterScheme = "zz-unparseable:qq";
    const { code, stderr } = await refusing(`postgres://${afterScheme}`);
    expect(
      { code, printed: stderr.includes(afterScheme) },
      "A string the masker could not parse reached stderr whole. A masker that fails open is " +
        "no masker: print a fixed placeholder for a string it cannot mask, and the reason."
    ).toEqual({ code: 2, printed: false });
  },
  refusalTimeout
);
