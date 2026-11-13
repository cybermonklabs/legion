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
 *  At legion/INFRA-1520 cef244c6de41 the masker learned the last `@` and a placeholder for what it
 *  cannot parse, and a password still reached stderr wherever its name was spelled in a way the
 *  masker did not match: `?Password=`, `?pass%57ord=` and the keyword `PASSWORD=`. libpq refuses
 *  each of those strings, but the leak comes first: the secret is on stderr whether or not the
 *  string would ever have connected. A masker decides there is no password by matching a name, and
 *  every near-miss name is a string it then prints in full, so the rule these rows hold is that a
 *  refusal names what is wrong and prints no part of the connection string, masked or not.
 *
 *  Every row asserts the two things that matter: the refusal (exit 2) and no secret on stderr. No
 *  row asserts a masked form such as `postgres://***@`, which the rule leaves nothing to print.
 *  The rows that held before this round hold the same measure as before.
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
  {
    shape: "a password parameter spelled with a capital letter",
    databaseUrl: "postgres://127.0.0.1:5433/?Password=sekrit&db%6Eame=mine",
    secret: "sekrit",
  },
  {
    shape: "a password parameter whose name escapes a capital letter",
    databaseUrl: "postgres://127.0.0.1:5433/?pass%57ord=sekrit&db%6Eame=mine",
    secret: "sekrit",
  },
  {
    shape: "the keyword form with the keyword in capitals",
    databaseUrl: "host=127.0.0.1 PASSWORD=sekrit dbname=mine",
    secret: "sekrit",
  },
]) {
  test(
    `a refusal writes no password to stderr: ${shape}`,
    async () => {
      const { code, stderr } = await refusing(databaseUrl);
      expect(
        { code, printed: stderr.includes(secret) },
        "The refusal wrote the password to stderr. A refusal names what is wrong and prints no " +
          "part of the connection string: a masker that decides by a name's spelling prints the " +
          "next spelling it does not know."
      ).toEqual({ code: 2, printed: false });
    },
    refusalTimeout
  );
}

test(
  "a refusal of a string no reader can parse names the reason and none of the string",
  async () => {
    // A person who meant user:password and left the host out: no `@` to find, no database for the
    // guard to read, so the guard refuses it, and no reading of the string can tell where a
    // password would stand.
    const afterScheme = "zz-unparseable:qq";
    const { code, stderr } = await refusing(`postgres://${afterScheme}`);
    expect(
      { code, printed: stderr.includes(afterScheme) },
      "The refused string reached stderr whole. A refusal names what is wrong and prints no part " +
        "of the connection string."
    ).toEqual({ code: 2, printed: false });
  },
  refusalTimeout
);
