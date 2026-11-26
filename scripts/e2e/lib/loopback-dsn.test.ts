import { describe, expect, test } from "bun:test";
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostGate, hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// loopback-dsn.py's tests: the helper reads each DSN with the real libpq of this host, so every
// case asserts the answer libpq gave the helper rather than a pattern of the DSN's text. The
// next block drives lib/loopback-dsn.sh's run_database_url, which the proofs call and lib/rig.sh
// sources, and pins the connection string it answers with. A host with no python3 fails them,
// naming it; a host with no libpq skips them naming it, and fails them so under GITHUB_ACTIONS.
// The last block runs lib/loopback-dsn.sh's helpers under `set -x` over stand-ins for python3
// and psql, and reads where the trace holds the DSN.

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
 *  fail every proof defines, and the `|| fail` each caller of the function writes. $1 is the
 *  checkout, and $2 to $5 are the function's own arguments. */
const rig = `set -euo pipefail
note() { :; }
fail() { echo "$*" >&2; exit 1; }
root=$1
evidence=''
work=''
timeout_hook=''
. "$root/scripts/e2e/lib/rig.sh"
run_database_url url "$2" "$3" "$4" "$5" || fail "$url"
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

// Every row runs the helper under python3, so a host without it fails each row by name, never
// skipping; the helper refuses every DSN where this host has no libpq, and says so in that
// refusal, which skips the rows outside CI and fails them by name under GITHUB_ACTIONS.
const host = hostGate({
  never: ["python3"],
  reason:
    hostTool("python3") !== null &&
    guard("postgres://127.0.0.1:5433/postgres").refusal.includes("no libpq")
      ? 'The helper reads each DSN with libpq, and ctypes.util.find_library("pq") finds none on this host; install libpq5.'
      : "",
});

host.describe("loopback-dsn.py takes a loopback DSN", () => {
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

  // The two DSNs a WHATWG `URL` reads another host from: it ends the user name at the last `@`
  // and starts the query at the first `?`, so it reads the hosts 10.1.2.3 and evil.example.
  // libpq ends the user name at the first `@` and starts no query before it, so the host of
  // both is localhost, and the guard takes them and prints that host. The clients the proofs
  // then connect with are libpq's own — psql, and the string run_database_url rebuilds below —
  // so each client agrees with the guard. Measured with psql 18.6: the first names the port
  // `5432@10.1.2.3`, which psql refuses with `invalid integer value "5432@10.1.2.3" for
  // connection option "port"` before it connects anywhere, and the second connects to
  // `localhost (127.0.0.1)` as the user `evil.example?`.
  test("a second `@` is part of the port, and no second host", () => {
    expect(guard("postgres://u@localhost:5432@10.1.2.3/db")).toEqual({
      code: 0,
      host: "localhost",
      refusal: "",
    });
  });

  test("a `?` right before the `@` starts no query", () => {
    expect(guard("postgres://evil.example?@localhost/db")).toEqual({
      code: 0,
      host: "localhost",
      refusal: "",
    });
  });

  // Host mode writes no value back, so it takes a password libpq reads as no UTF-8 text: libpq
  // connects with the bytes the DSN gives it, whatever they are. The --dbname mode refuses this
  // DSN, in the last block below.
  test("a password that is no UTF-8 text is still a loopback host", () => {
    expect(guard("postgres://u:p%ffq@localhost/db")).toEqual({
      code: 0,
      host: "localhost",
      refusal: "",
    });
  });
});

host.describe("loopback-dsn.py refuses the host libpq reads", () => {
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

host.describe("loopback-dsn.py refuses what it cannot read as one host", () => {
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

host.describe("lib/loopback-dsn.sh's run_database_url names the run's own database", () => {
  const database = "ppd_7_head_1_1";
  // Each row is a DSN the guard takes, the connection string the function answers with, and the
  // host the DSN names. The first two hold a `?` inside the user name, where libpq starts no
  // query, and the third's text ends in a `?`: a `?dbname=` or `&dbname=` appended to the text
  // of any of the three names another database or parses no more. The helper reads the string
  // back with libpq before it prints it, so a row that passes is a string libpq reads as the
  // DSN's own keywords with this database.
  // The last three are what a text join of the DSN's parts gets wrong. In the fifth a WHATWG
  // `URL` reads the host 10.1.2.3 and in the sixth evil.example, where libpq reads localhost in
  // both; the rebuilt string carries libpq's reading, which is the host the guard printed. The
  // seventh sets dbname twice over, in its path and in its query, and libpq and pgx both take
  // the query's: the rebuild carries one dbname, which is the run's own database.
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
    [
      "postgres://u@localhost:5432@10.1.2.3/db",
      `dbname='${database}' host='localhost' port='5432@10.1.2.3' user='u'`,
      "localhost",
    ],
    [
      "postgres://evil.example?@localhost/db",
      `dbname='${database}' host='localhost' user='evil.example?'`,
      "localhost",
    ],
    [
      "postgres://u@127.0.0.1:5433/postgres?dbname=other&sslmode=disable",
      `dbname='${database}' host='127.0.0.1' port='5433' sslmode='disable' user='u'`,
      "127.0.0.1",
    ],
  ];
  for (const [dsn, string, host] of rows) {
    test(`${dsn} is rewritten to that database on ${host}`, () => {
      expect(rewrite(dsn, database)).toEqual({ code: 0, url: string, refusal: "" });
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

  // libpq decodes a percent sign, so each of these two reaches the rewrite as a password the
  // rebuilt string cannot carry, and one check refuses both: a character that does not print.
  // The first is the byte 0xFF, which the decode keeps as the surrogate U+DCFF, and a file
  // carries no surrogate back to psql. The second is a line break, which a caller's
  // configuration file reads as the end of the value. Host mode takes the first of the two, in
  // the first block above: it writes no value back.
  test("a password libpq reads as no UTF-8 text is refused by its keyword", () => {
    const answer = rewrite("postgres://u:p%ffq@localhost/db", database);
    expect(answer.code).toBe(1);
    expect(answer.url).toBe("");
    expect(answer.refusal).toBe(
      `${variable} gives password U+DCFF, a character that does not print: it reaches the` +
        " rewritten connection string, where a caller writes that string into a file, and this" +
        " refusal prints no value that holds one"
    );
  });

  test("a percent-encoded newline in the password is refused by its keyword", () => {
    const answer = rewrite("postgres://u:p%0aq@localhost/db", database);
    expect(answer.code).toBe(1);
    expect(answer.url).toBe("");
    expect(answer.refusal).toBe(
      `${variable} gives password U+000A, a character that does not print: it reaches the` +
        " rewritten connection string, where a caller writes that string into a file, and this" +
        " refusal prints no value that holds one"
    );
  });
});

/** The shell a traced case runs: the lib sourced, then the case's CALL under `set -x`, with the
 *  DSN in the environment as `variable` and stand-ins for python3 and psql first on PATH. $1 is
 *  the checkout, and $2 the call. */
const tracedRig = `set -euo pipefail
. "$1/scripts/e2e/lib/loopback-dsn.sh"
set -x
eval "$2"
set +x
printf '%s\\n' "\${answer-}"
`;
const scratch = temporaryDirectories("loopback-dsn-test");

/** Runs call under `set -x` (tracedRig), where python3 prints 127.0.0.1 and psql prints the
 *  connection string it was given and succeeds, with dsn in `variable`: its exit status, the trace
 *  (its stderr) and its stdout, which is what psql printed where the call leaves psql's stdout
 *  alone, then what the call assigned `answer`. */
function traced(call: string, dsn: string): { code: number; trace: string; answer: string } {
  const bin = scratch();
  const standIns: [string, string][] = [
    ["python3", "echo 127.0.0.1"],
    ["psql", `printf '%s\\n' "$1"`],
  ];
  for (const [name, body] of standIns) {
    writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`);
    chmodSync(join(bin, name), 0o755);
  }
  const run = Bun.spawnSync(["bash", "-c", tracedRig, "traced", root, call], {
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, [variable]: dsn },
    stderr: "pipe",
    stdout: "pipe",
  });
  return {
    code: run.exitCode,
    trace: run.stderr.toString(),
    answer: run.stdout.toString().trim(),
  };
}

// Each helper turns the shell's xtrace off for itself, so under `bash -x` the DSN's password is
// on no line of its own trace. A caller's own line that passes the DSN still carries it, which
// require_loopback_env, run_database_url_env and psql_env avoid by taking the variable's name:
// their calls hold no DSN at all.
describe("lib/loopback-dsn.sh's helpers trace no DSN of their own", () => {
  const dsn = `postgres://legion_test:${password}@127.0.0.1:5433/postgres?sslmode=disable`;

  const byName: [string, string][] = [
    ["require_loopback_env", `require_loopback_env answer ${variable} "${what}"`],
    ["run_database_url_env", `run_database_url_env answer ${variable} ppd_1 "${what}"`],
  ];
  for (const [name, call] of byName) {
    test(`${name} reads the DSN by its variable's name, and the trace holds none of it`, () => {
      const run = traced(call, dsn);
      expect(run.trace).toContain(`+ ${name} answer ${variable} `);
      expect(run.trace).not.toContain(password);
      expect(run.code).toBe(0);
      // The stand-in python3 answers 127.0.0.1 for the host and the rebuilt string alike.
      expect(run.answer).toBe("127.0.0.1");
    });
  }

  test("psql_env reads the DSN by its variable's name, and the trace holds none of it", () => {
    const run = traced(`psql_env ${variable} -X -q -c "select 1"`, dsn);
    expect(run.trace).toContain(`+ psql_env ${variable} -X -q -c 'select 1'`);
    expect(run.trace).not.toContain(password);
    expect(run.code).toBe(0);
    // The stand-in psql printed the connection string it was given, which is the DSN itself.
    expect(run.answer).toBe(dsn);
  });

  const callers: [string, string][] = [
    ["require_loopback_dsn", `require_loopback_dsn answer ${variable} "$${variable}" "${what}"`],
    ["run_database_url", `run_database_url answer ${variable} "$${variable}" ppd_1 "${what}"`],
    ["empty_public_schema", `empty_public_schema "$${variable}"`],
  ];
  for (const [name, call] of callers) {
    test(`${name} holds the password on its caller's line alone`, () => {
      const run = traced(call, dsn);
      const holding = run.trace.split("\n").filter((line) => line.includes(password));
      expect(holding.length).toBe(1);
      // The case's call runs through eval, which traces it one `+` deeper than eval's own line.
      expect(holding[0]).toMatch(new RegExp(`^\\++ ${name} `));
      expect(run.code).toBe(0);
    });
  }
});
