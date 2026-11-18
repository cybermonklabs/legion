import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// loopback-dsn.py's tests: the helper reads each DSN with the real libpq of this host, so every
// case asserts the answer libpq gave the helper rather than a pattern of the DSN's text. The
// blocks after the host mode's drive the --dbname forms through lib/loopback-dsn.sh's
// exec_run_database and require_run_database, which the proofs call and lib/rig.sh sources. A
// case that pins the connection string, the URI or libpq's variables reads them out of the
// environment of the command the helper ran, which `env -0` writes to a file of the test's own,
// and never out of anything the helper printed: it prints none of them. A host with no python3 or
// no libpq skips them, naming what it lacks.

const root = join(import.meta.dir, "..", "..", "..");
const helper = join(import.meta.dir, "loopback-dsn.py");
const variable = "LOOPBACK_DSN_TEST";
/** The VAR the --env forms hand the run's own database in. */
const handedVariable = "LOOPBACK_DSN_TEST_URL";
const what = "this run creates and drops databases there";
/** A password no refusal may print. It is in the userinfo of each refused DSN that carries one. */
const password = "hunter2";
/** A password that needs percent-encoding, encoded as a client writes it: `a b'c@d`. */
const spacedPassword = encodeURIComponent("a b'c@d").replaceAll("'", "%27");
/** A password that holds a newline, percent-encoded: `p`, U+000A, `q`. */
const newlinePassword = encodeURIComponent("p\nq").toLowerCase();
const files = temporaryDirectories("loopback-dsn");

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

/** What a shell that sources the rig as a proof does, with the note and the fail every proof
 *  defines, answered when it ran body: its exit status, its stdout and its stderr. $1 of body
 *  is args' first word. env replaces this process's environment for the shell. */
function sourced(
  body: string,
  args: string[],
  env: Record<string, string | undefined> = process.env
): { code: number; stdout: string; stderr: string } {
  const script = `set -euo pipefail
note() { :; }
fail() { echo "$*" >&2; exit 1; }
root=$1
shift
evidence=''
work=''
timeout_hook=''
. "$root/scripts/e2e/lib/rig.sh"
${body}`;
  const run = Bun.spawnSync(["bash", "-c", script, "rig", root, ...args], {
    env,
    stderr: "pipe",
    stdout: "pipe",
  });
  return { code: run.exitCode, stdout: run.stdout.toString(), stderr: run.stderr.toString() };
}

/** The environment `env -0` wrote to file, each variable by its name, or null for a file the
 *  command never wrote to: a refused DSN runs no command. */
function writtenEnvironment(file: string): Map<string, string> | null {
  const text = readFileSync(file, "utf8");
  if (text === "") return null;
  const environment = new Map<string, string>();
  for (const entry of text.split("\0").filter((line) => line !== "")) {
    const equals = entry.indexOf("=");
    environment.set(entry.slice(0, equals), entry.slice(equals + 1));
  }
  return environment;
}

/** What exec_run_database handed `env -0` for dsn and database in form, the words between
 *  WHAT and `--`: the shell's exit status, the command's whole environment, null when no command
 *  ran, and the shell's stderr, which is the helper's refusal line when it refused. A subshell
 *  whose stdout is a file of this test's own runs the function, as a proof runs it in `( … )`;
 *  env is the environment of the shell that sources the rig. */
function handed(
  dsn: string,
  database: string,
  form: string[],
  env: Record<string, string | undefined> = process.env
): { code: number; environment: Map<string, string> | null; refusal: string } {
  const file = join(files(), "environment");
  const run = sourced(
    `out=$1 dsn=$2 database=$3 what=$4
shift 4
(exec_run_database ${variable} "$dsn" "$database" "$what" "$@" -- env -0) >"$out"`,
    [file, dsn, database, what, ...form],
    env
  );
  return { code: run.code, environment: writtenEnvironment(file), refusal: run.stderr.trim() };
}

/** What the connection string's form, or with uri the URI's, handed its command in VAR, as the
 *  rows below read it: the exit status, whether a command ran at all, VAR's value, and the
 *  refusal line. */
function rewrite(
  dsn: string,
  database: string,
  uri = false
): { code: number; ran: boolean; url: string | undefined; refusal: string } {
  const answer = handed(dsn, database, [...(uri ? ["--uri"] : []), "--env", handedVariable]);
  return {
    code: answer.code,
    ran: answer.environment !== null,
    url: answer.environment?.get(handedVariable),
    refusal: answer.refusal,
  };
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

  // The two DSNs a WHATWG `URL` reads another host from: it ends the user name at the last `@`
  // and starts the query at the first `?`, so it reads the hosts 10.1.2.3 and evil.example.
  // libpq ends the user name at the first `@` and starts no query before it, so the host of
  // both is localhost, and the guard takes them and prints that host. The clients the proofs
  // then connect with are libpq's own — psql, and the string exec_run_database hands on below —
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
  // connects with the bytes the DSN gives it, whatever they are. The --dbname forms refuse this
  // DSN, in the exec_run_database --env block below.
  test("a password that is no UTF-8 text is still a loopback host", () => {
    expect(guard("postgres://u:p%ffq@localhost/db")).toEqual({
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

describe.skipIf(skip)("exec_run_database --env hands its command the run's own database", () => {
  const database = "ppd_7_head_1_1";
  // Each row is a DSN the guard takes, the connection string the command is handed in VAR, and
  // the host the DSN names. The first two hold a `?` inside the user name, where libpq starts no
  // query, and the third's text ends in a `?`: a `?dbname=` or `&dbname=` appended to the text
  // of any of the three names another database or parses no more. The helper reads the string
  // back with libpq before it runs the command, so a row that passes is a string libpq reads as
  // the DSN's own keywords with this database.
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
      expect(rewrite(dsn, database)).toEqual({ code: 0, ran: true, url: string, refusal: "" });
    });
  }

  test("a DSN the guard refuses is refused by name, and no command runs", () => {
    const answer = rewrite(`postgres://legion_test:${password}@db.prod.example/db`, database);
    expect(answer.code).toBe(1);
    expect(answer.ran).toBe(false);
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host db.prod.example,` +
        ` and ${what}`
    );
  });

  // libpq decodes a percent sign, so each of these two reaches the rewrite as a password no
  // command is handed, and one check refuses both: a character that does not print. The first is
  // the byte 0xFF, which the decode keeps as the surrogate U+DCFF, which the URI form cannot
  // percent-encode. The second is a line break, which ends a line of whatever writes the
  // command's environment out. Host mode takes the first of the two, in the first block above:
  // it hands no value on.
  test("a password libpq reads as no UTF-8 text is refused by its keyword", () => {
    const answer = rewrite("postgres://u:p%ffq@localhost/db", database);
    expect(answer.code).toBe(1);
    expect(answer.ran).toBe(false);
    expect(answer.refusal).toBe(
      `${variable} gives password U+DCFF, a character that does not print: every value the DSN` +
        " sets reaches the command's environment, and this refusal prints no value that holds one"
    );
  });

  test("a percent-encoded newline in the password is refused by its keyword", () => {
    const answer = rewrite("postgres://u:p%0aq@localhost/db", database);
    expect(answer.code).toBe(1);
    expect(answer.ran).toBe(false);
    expect(answer.refusal).toBe(
      `${variable} gives password U+000A, a character that does not print: every value the DSN` +
        " sets reaches the command's environment, and this refusal prints no value that holds one"
    );
  });
});

describe.skipIf(skip)(
  "exec_run_database --uri --env hands its command the run's own database as a URI",
  () => {
    const database = "ppd_7_head_1_1";
    // The rows of the block above and two more, each with the URI the command is handed and
    // the host a URL parser reads from that URI, which is the host libpq reads from the DSN. A
    // consumer that takes the URI reads it with a URL parser (Go's net/url under pgx, the WHATWG
    // `URL` here), so a `?` or `=` of the user name is percent-encoded and ends nothing there, and
    // ::1 is written in brackets. The port `5432@10.1.2.3` libpq reads in the fifth reaches the URI
    // percent-encoded, where libpq reads it back the same and a URL parser refuses the URI whole
    // (null), so a consumer of either kind connects nowhere else. The last row's password holds a
    // space, a single quote and an `@`, each encoded.
    const rows: [string, string, string | null][] = [
      [
        "postgres://u?x:p@localhost:5433/db",
        `postgres://u%3Fx:p@localhost:5433/${database}`,
        "localhost",
      ],
      [
        "postgres://evil.example?x=@localhost",
        `postgres://evil.example%3Fx%3D@localhost/${database}`,
        "localhost",
      ],
      ["postgres://localhost/db?", `postgres://localhost/${database}`, "localhost"],
      [
        "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
        `postgres://legion_test@127.0.0.1:5433/${database}?sslmode=disable`,
        "127.0.0.1",
      ],
      [
        "postgres://u@localhost:5432@10.1.2.3/db",
        `postgres://u@localhost:5432%4010.1.2.3/${database}`,
        null,
      ],
      [
        "postgres://evil.example?@localhost/db",
        `postgres://evil.example%3F@localhost/${database}`,
        "localhost",
      ],
      [
        "postgres://u@127.0.0.1:5433/postgres?dbname=other&sslmode=disable",
        `postgres://u@127.0.0.1:5433/${database}?sslmode=disable`,
        "127.0.0.1",
      ],
      ["postgres://u:p@[::1]:5432/db", `postgres://u:p@[::1]:5432/${database}`, "[::1]"],
      [
        `postgres://u:${spacedPassword}@localhost/db`,
        `postgres://u:${spacedPassword}@localhost/${database}`,
        "localhost",
      ],
    ];
    for (const [dsn, uri, host] of rows) {
      test(`${dsn} is rewritten to that database as ${uri}`, () => {
        expect(rewrite(dsn, database, true)).toEqual({ code: 0, ran: true, url: uri, refusal: "" });
        expect(URL.canParse(uri) ? new URL(uri).hostname : null).toBe(host);
      });
    }

    test("a DSN the guard refuses is refused by name, and no command runs", () => {
      const answer = rewrite(
        `postgres://legion_test:${password}@db.prod.example/db`,
        database,
        true
      );
      expect(answer.code).toBe(1);
      expect(answer.ran).toBe(false);
      expect(answer.refusal).toBe(
        `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host db.prod.example,` +
          ` and ${what}`
      );
    });

    test("a percent-encoded newline in the password is refused by its keyword", () => {
      const answer = rewrite(`postgres://u:${newlinePassword}@localhost/db`, database, true);
      expect(answer.code).toBe(1);
      expect(answer.ran).toBe(false);
      expect(answer.refusal).toBe(
        `${variable} gives password U+000A, a character that does not print: every value the DSN` +
          " sets reaches the command's environment, and this refusal prints no value that holds one"
      );
    });
  }
);

describe.skipIf(skip)("the --check form prints nothing of a DSN it takes", () => {
  const database = "ppd_7_head_1_1";
  const dsn = `postgres://legion_test:${password}@127.0.0.1:5433/postgres?sslmode=disable`;
  const remote = `postgres://legion_test:${password}@db.prod.example/db`;
  const refusal =
    `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host db.prod.example,` +
    ` and ${what}`;

  /** What `loopback-dsn.py … --dbname DATABASE FORM --check` answered for dsn: its exit status,
   *  its stdout and its stderr. */
  function check(dsn: string, form: string[]): { code: number; stdout: string; stderr: string } {
    const run = Bun.spawnSync(
      ["python3", helper, variable, what, "--dbname", database, ...form, "--check"],
      { env: { ...process.env, [variable]: dsn }, stderr: "pipe", stdout: "pipe" }
    );
    return { code: run.exitCode, stdout: run.stdout.toString(), stderr: run.stderr.toString() };
  }

  for (const form of [[], ["--uri"], ["--libpq-env"]]) {
    test(`${[...form, "--check"].join(" ")} prints nothing for a DSN it takes`, () => {
      expect(check(dsn, form)).toEqual({ code: 0, stdout: "", stderr: "" });
    });
  }

  test("--check prints the refusal alone, on stderr, for a DSN the guard refuses", () => {
    expect(check(remote, [])).toEqual({ code: 1, stdout: "", stderr: `${refusal}\n` });
  });

  test("require_run_database assigns nothing for a DSN it takes and the refusal for one it refuses", () => {
    const body = `dsn=$1 database=$2 what=$3
shift 3
status=0
require_run_database answer ${variable} "$dsn" "$database" "$what" "$@" || status=$?
printf '%s [%s]' "$status" "$answer"`;
    expect(sourced(body, [dsn, database, what])).toEqual({ code: 0, stdout: "0 []", stderr: "" });
    expect(sourced(body, [dsn, database, what, "--libpq-env"])).toEqual({
      code: 0,
      stdout: "0 []",
      stderr: "",
    });
    expect(sourced(body, [remote, database, what, "--uri"])).toEqual({
      code: 0,
      stdout: `1 [${refusal}]`,
      stderr: "",
    });
  });
});

describe.skipIf(skip)("exec_run_database hands its command the database and nothing more", () => {
  const database = "ppd_7_head_1_1";
  const dsn = `postgres://legion_test:${password}@127.0.0.1:5433/postgres?sslmode=disable`;
  const string =
    `dbname='${database}' host='127.0.0.1' password='${password}' port='5433'` +
    ` sslmode='disable' user='legion_test'`;
  /** This process's environment without LANG and the LC_ variables, so that Python starts under
   *  the C locale and sets LC_CTYPE in its own environment (PEP 538). */
  const cLocale = Object.fromEntries(
    Object.entries(process.env).filter(([name]) => name !== "LANG" && !name.startsWith("LC_"))
  );

  // `_` is the one variable bash sets for each command it runs, to that command's path, so it
  // differs between the two commands this case compares and is left out of both.
  test("--env's command gets the shell's environment with VAR, without NAME, and with nothing Python set for itself", () => {
    const directory = files();
    const run = sourced(
      `out=$1 base=$2 dsn=$3 database=$4 what=$5
(exec env -0) >"$base"
(exec_run_database ${variable} "$dsn" "$database" "$what" --env ${handedVariable} -- env -0) >"$out"`,
      [join(directory, "out"), join(directory, "base"), dsn, database, what],
      cLocale
    );
    expect(run).toEqual({ code: 0, stdout: "", stderr: "" });
    const base = writtenEnvironment(join(directory, "base")) ?? new Map<string, string>();
    const got = writtenEnvironment(join(directory, "out")) ?? new Map<string, string>();
    base.delete("_");
    got.delete("_");
    expect(base.has(variable)).toBe(false);
    expect(base.has("LC_CTYPE")).toBe(false);
    expect(got.get(handedVariable)).toBe(string);
    got.delete(handedVariable);
    expect(Object.fromEntries(got)).toEqual(Object.fromEntries(base));
  });

  test("--env with NAME as VAR hands the string under the name the caller read the DSN from", () => {
    const answer = handed(dsn, database, ["--env", variable]);
    expect(answer.code).toBe(0);
    expect(answer.environment?.get(variable)).toBe(string);
  });

  test("--env-only's command gets VAR and nothing else", () => {
    const answer = handed(dsn, database, ["--env-only", handedVariable]);
    expect(answer.code).toBe(0);
    expect([...(answer.environment ?? [])]).toEqual([[handedVariable, string]]);
    const uri = handed(dsn, database, ["--uri", "--env-only", handedVariable], cLocale);
    expect(uri.code).toBe(0);
    expect([...(uri.environment ?? [])]).toEqual([
      [
        handedVariable,
        `postgres://legion_test:${password}@127.0.0.1:5433/${database}?sslmode=disable`,
      ],
    ]);
  });

  for (const form of [
    ["--env", handedVariable],
    ["--uri", "--env", handedVariable],
    ["--env-only", handedVariable],
    ["--uri", "--env-only", handedVariable],
    ["--libpq-env"],
  ]) {
    test(`${form.join(" ")} prints no part of the password`, () => {
      const run = sourced(
        `dsn=$1 database=$2 what=$3
shift 3
(exec_run_database ${variable} "$dsn" "$database" "$what" "$@" -- true)`,
        [dsn, database, what, ...form]
      );
      expect(run.stdout).not.toContain(password);
      expect(run.stderr).not.toContain(password);
      expect(run).toEqual({ code: 0, stdout: "", stderr: "" });
    });
  }

  test("the command keeps the pid the shell's $! names", () => {
    const file = join(files(), "pid");
    const run = sourced(
      `file=$1 dsn=$2 database=$3 what=$4
exec_run_database ${variable} "$dsn" "$database" "$what" --env ${handedVariable} -- \\
  sh -c 'printf "%s" "$$" >"$0"' "$file" &
pid=$!
wait "$pid"
printf '%s' "$pid"`,
      [file, dsn, database, what]
    );
    expect(run.code).toBe(0);
    expect(run.stdout).toMatch(/^[1-9][0-9]*$/);
    expect(readFileSync(file, "utf8")).toBe(run.stdout);
  });

  test("exec_run_database refuses to replace the script's own shell", () => {
    const run = sourced(
      `exec_run_database ${variable} "$1" db "$2" --env ${handedVariable} -- true || printf 'returned %s' "$?"`,
      [dsn, what]
    );
    expect(run).toEqual({
      code: 0,
      stdout: "returned 2",
      stderr:
        "exec_run_database runs its command in place of the shell that calls it, and this is" +
        " the script's own shell: call it in ( … ) or with &\n",
    });
  });
});

describe.skipIf(skip)("exec_run_database --libpq-env hands psql libpq's own variables", () => {
  const database = "ppd_7_head_1_1";
  /** This process's environment without a variable libpq reads, and with PGSERVICE, which names a
   *  ~/.pg_service.conf entry the command must not be handed. */
  const withService = {
    ...Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.startsWith("PG"))),
    PGSERVICE: "elsewhere",
  };

  test("each keyword is the variable libpq reads it from, and NAME and PGSERVICE are not handed on", () => {
    const answer = handed(
      `postgres://legion_test:${password}@127.0.0.1:5433/postgres?sslmode=disable&application_name=proof`,
      database,
      ["--libpq-env"],
      withService
    );
    expect(answer.code).toBe(0);
    expect(answer.refusal).toBe("");
    expect(answer.environment?.has(variable)).toBe(false);
    expect(
      [...(answer.environment ?? [])].filter(([name]) => name.startsWith("PG")).sort()
    ).toEqual([
      ["PGAPPNAME", "proof"],
      ["PGDATABASE", database],
      ["PGHOST", "127.0.0.1"],
      ["PGPASSWORD", password],
      ["PGPORT", "5433"],
      ["PGSSLMODE", "disable"],
      ["PGUSER", "legion_test"],
    ]);
  });

  test("a keyword libpq reads from no variable is refused by name, and no command runs", () => {
    const answer = handed(
      `postgres://legion_test:${password}@127.0.0.1:5433/postgres?keepalives=1`,
      database,
      ["--libpq-env"]
    );
    expect(answer).toEqual({
      code: 1,
      environment: null,
      refusal:
        `${variable} sets keepalives, which libpq reads from no environment variable:` +
        " --libpq-env hands the command every keyword the DSN sets in the variable libpq reads" +
        " it from",
    });
  });
});
