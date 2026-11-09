import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";

// loopback-dsn.py's tests: the helper reads each DSN with the real libpq of this host, so every
// case asserts the answer libpq gave the helper rather than a pattern of the DSN's text. The
// last block drives lib/rig.sh's run_database_url, which the proofs call, and reads its answer
// back through the helper. A host with no python3 or no libpq skips them, naming what it lacks.

const root = join(import.meta.dir, "..", "..", "..");
const helper = join(import.meta.dir, "loopback-dsn.py");
const variable = "LOOPBACK_DSN_TEST";
const what = "this run creates and drops databases there";
/** A password no refusal may print. It is in the userinfo of each refused DSN that carries one. */
const password = "hunter2";

/** What the helper answered: its exit status, the host it printed, and its refusal line. */
function guard(dsn: string): { code: number; host: string; refusal: string } {
  const run = Bun.spawnSync(["python3", helper, variable, what], {
    env: { ...process.env, [variable]: dsn },
    stderr: "pipe",
    stdout: "pipe",
  });
  return {
    code: run.exitCode,
    host: run.stdout.toString().trim(),
    refusal: run.stderr.toString().trim(),
  };
}

/** The shell a run_database_url case runs: the rig as a proof sources it, with the note and the
 *  fail every proof defines. $1 is the checkout, and $2 to $5 are the function's own arguments. */
const rig = `set -euo pipefail
note() { :; }
fail() { echo "$*" >&2; exit 1; }
root=$1
evidence=''
work=''
timeout_hook=''
. "$root/scripts/e2e/lib/rig.sh"
run_database_url url "$2" "$3" "$4" "$5"
printf '%s\\n' "$url"
`;

/** What run_database_url answered for dsn and database: its exit status, the connection string
 *  it assigned, and the line it failed with. */
function rewrite(dsn: string, database: string): { code: number; url: string; refusal: string } {
  const run = Bun.spawnSync(["bash", "-c", rig, "rig", root, variable, dsn, database, what], {
    stderr: "pipe",
    stdout: "pipe",
  });
  return {
    code: run.exitCode,
    url: run.stdout.toString().trim(),
    refusal: run.stderr.toString().trim(),
  };
}

/** The host and the database libpq reads from a connection string (`--read`). */
function reads(url: string): { host: string; dbname: string } {
  const run = Bun.spawnSync(["python3", helper, variable, what, "--read"], {
    env: { ...process.env, [variable]: url },
    stderr: "pipe",
    stdout: "pipe",
  });
  if (run.exitCode !== 0) {
    throw new Error(run.stderr.toString().trim());
  }
  const read: Record<string, string> = {};
  for (const line of run.stdout.toString().trim().split("\n")) {
    read[line.slice(0, line.indexOf("="))] = line.slice(line.indexOf("=") + 1);
  }
  return { host: read.host, dbname: read.dbname };
}

const withoutPython = skipWithout("python3");
// The helper refuses every DSN where this host has no libpq, and says so in that refusal.
const withoutLibpq =
  !withoutPython && guard("postgres://127.0.0.1:5433/postgres").refusal.includes("no libpq");
if (withoutLibpq) {
  console.warn(
    `Skipped the tests in ${import.meta.path}. The helper reads each DSN with libpq, and ` +
      `ctypes.util.find_library("pq") finds none on this host; install libpq5.`
  );
}
const skip = withoutPython || withoutLibpq;

describe.skipIf(skip)("loopback-dsn.py takes a loopback DSN", () => {
  test("the DSN a run is given prints its host", () => {
    const answer = guard(`postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable`);
    expect(answer).toEqual({ code: 0, host: "127.0.0.1", refusal: "" });
  });

  test("localhost and ::1 are loopback too", () => {
    expect(guard("postgres://localhost:5433/db")).toEqual({
      code: 0,
      host: "localhost",
      refusal: "",
    });
    expect(guard("postgres://[::1]:5433/db")).toEqual({ code: 0, host: "::1", refusal: "" });
  });

  test("a loopback host given as a parameter is the host libpq reads", () => {
    expect(guard("postgres://localhost:5433/db?host=127.0.0.1")).toEqual({
      code: 0,
      host: "127.0.0.1",
      refusal: "",
    });
  });

  // libpq ends the user name at the first `@` and a `?` does not end it, so the user name here
  // is `evil.example?x=` and the host is localhost. pgx v5 reads the same two.
  test("a host name inside the user name is no host", () => {
    expect(guard("postgres://evil.example?x=@localhost")).toEqual({
      code: 0,
      host: "localhost",
      refusal: "",
    });
  });

  test("an empty hostaddr parameter is no hostaddr", () => {
    expect(guard("postgres://localhost:5433/db?hostaddr=")).toEqual({
      code: 0,
      host: "localhost",
      refusal: "",
    });
  });
});

describe.skipIf(skip)("loopback-dsn.py refuses the host libpq reads", () => {
  // libpq ends the user name at the first `@`, and a `?` does not end it, so the host of each of
  // these two is evil.example while the authority up to the first `?` reads localhost.
  test("a `?` before the `@` does not end the user name", () => {
    const answer = guard(`postgres://localhost?sslmode=disable:${password}@evil.example/postgres`);
    expect(answer.code).toBe(1);
    expect(answer.host).toBe("");
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host evil.example,` +
        ` and ${what}`
    );
  });

  test("a `?` right before the password does not end the user name", () => {
    const answer = guard(`postgres://localhost?:${password}@evil.example:5432/db`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host evil.example,` +
        ` and ${what}`
    );
  });

  test("a percent-encoded host key reaches libpq as a host", () => {
    const answer = guard(
      `postgres://legion_test:${password}@localhost:5433/db?%68ost=evil.example`
    );
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host evil.example,` +
        ` and ${what}`
    );
  });

  test("a remote host is refused by its own name", () => {
    const answer = guard(`postgres://legion_test:${password}@db.prod.example:5432/postgres`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host db.prod.example,` +
        ` and ${what}`
    );
  });

  test("a DSN with no host at all is refused", () => {
    const answer = guard("postgres:///postgres");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} names no host: libpq would read PGHOST or its own default, and ${what}`
    );
  });

  test("a Unix-socket directory is no loopback host", () => {
    const answer = guard("postgres:///db?host=/var/run/postgresql");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host` +
        ` /var/run/postgresql, and ${what}`
    );
  });
});

describe.skipIf(skip)("loopback-dsn.py refuses what it cannot read as one host", () => {
  // Each DSN below carries the password in its userinfo, and each refusal is read back whole, so
  // the assertion itself says that no part of that userinfo reached the refusal.
  test("a newline hides the `host=` key from a line reader, not from libpq", () => {
    const answer = guard(
      `postgres://legion_test:${password}@localhost:5433/db?application_name=x\n&host=evil.example`
    );
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} holds U+000A, a character that does not print: libpq reads the whole DSN,` +
        " and a reader that stops at the end of a line reads less of it"
    );
  });

  test("two hosts are two servers a drop can reach", () => {
    const answer = guard(`postgres://legion_test:${password}@localhost,evil.example:5432/db`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must name one host: libpq reads 2 (localhost, evil.example), and it connects` +
        " to the next one when the first does not answer"
    );
  });

  test("hostaddr names the server the host name does not", () => {
    const answer = guard(`postgres://legion_test:${password}@localhost:5433/db?hostaddr=10.0.0.1`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must carry no hostaddr parameter: libpq reads hostaddr=10.0.0.1 and connects` +
        " to that address, whatever the host name is"
    );
  });

  test("service reads a host out of ~/.pg_service.conf", () => {
    const answer = guard(`postgres://legion_test:${password}@localhost:5433/db?service=prod`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must carry no service parameter: libpq reads service=prod from` +
        " ~/.pg_service.conf, and that entry can set hostaddr, which sends the connection to" +
        " another address"
    );
  });

  test("a DSN libpq cannot parse is refused, and its text is not printed", () => {
    const answer = guard(`postgres://legion_test:${password}@localhost:5433/db?=x`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(`${variable} is no connection string libpq can parse`);
  });

  // libpq decodes a percent sign, so each of these three reaches it as a host holding a
  // character that does not print.
  test("a percent-encoded newline in the host is refused in one line", () => {
    const answer = guard(`postgres://legion_test:${password}@evil%0aFAKE%20LINE/db`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} gives host U+000A, a character that does not print: libpq decodes a percent` +
        " sign in the DSN, and this refusal prints no value that holds one"
    );
  });

  test("a percent-encoded escape sequence in the host is never written out", () => {
    const answer = guard(`postgres://legion_test:${password}@%1b%5b2J/db`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} gives host U+001B, a character that does not print: libpq decodes a percent` +
        " sign in the DSN, and this refusal prints no value that holds one"
    );
  });

  // U+009B is the 8-bit CSI: one character that begins an escape sequence, which a check for the
  // C0 controls and DEL alone lets through to the refusal line.
  test("a percent-encoded C1 control in the host is never written out", () => {
    const answer = guard(`postgres://legion_test:${password}@evil%c2%9b2J.example/db`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} gives host U+009B, a character that does not print: libpq decodes a percent` +
        " sign in the DSN, and this refusal prints no value that holds one"
    );
  });

  test("an unset variable is refused by name", () => {
    const run = Bun.spawnSync(["python3", helper, variable, what], {
      env: { ...process.env, [variable]: undefined },
      stderr: "pipe",
      stdout: "pipe",
    });
    expect(run.exitCode).toBe(1);
    expect(run.stderr.toString().trim()).toBe(`${variable} is not set`);
  });
});

describe.skipIf(skip)("lib/rig.sh's run_database_url names the run's own database", () => {
  const database = "ppd_7_head_1_1";
  // Each row is a DSN the guard takes, the connection string the function answers with, and the
  // host libpq reads from that string. The first two hold a `?` inside the user name, where
  // libpq starts no query, and the third's text ends in a `?`: a `?dbname=` or `&dbname=`
  // appended to the text of any of the three names another database or parses no more.
  const rows: [string, string, string][] = [
    [
      "postgres://u?x:p@localhost:5433/db",
      `dbname='${database}' host='localhost' password='p' port='5433' user='u?x'`,
      "localhost",
    ],
    [
      "postgres://evil.example?x=@localhost",
      `dbname='${database}' host='localhost' user='evil.example?x='`,
      "localhost",
    ],
    ["postgres://localhost/db?", `dbname='${database}' host='localhost'`, "localhost"],
    [
      "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
      `dbname='${database}' host='127.0.0.1' port='5433' sslmode='disable' user='legion_test'`,
      "127.0.0.1",
    ],
  ];
  for (const [dsn, string, host] of rows) {
    test(`${dsn} is rewritten to that database on ${host}`, () => {
      const answer = rewrite(dsn, database);
      expect(answer).toEqual({ code: 0, url: string, refusal: "" });
      expect(reads(answer.url)).toEqual({ host, dbname: database });
    });
  }

  test("a DSN the guard refuses is refused by name, and no string is printed", () => {
    const answer = rewrite(`postgres://legion_test:${password}@db.prod.example/db`, database);
    expect(answer.code).toBe(1);
    expect(answer.url).toBe("");
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host db.prod.example,` +
        ` and ${what}`
    );
  });
});
