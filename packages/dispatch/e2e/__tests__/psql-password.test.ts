import { afterAll, beforeAll, expect, test } from "bun:test";
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { delimiter, join } from "node:path";
import { captureEnvironment } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { handOverMessage } from "../local-instance-support";
import { DatabaseUrlRefused, splitDatabaseUrl } from "../pg-url";
import { resetSession, sql, sqlAt } from "../psql";

/** A stand-in psql first on PATH, which writes each argument it was given on a line of its own
 *  and the PGPASSWORD it was given (`unset` for none), and exits. The rows read what psql was
 *  handed rather than what a server answered, so they need no Postgres. The `beforeAll` below,
 *  registered after the helper's own, writes it into the directory. */
const standIn = temporaryDirectory("psql-password");
const record = join(standIn, "record");

/** What the stand-in psql was last handed. */
function handed(): { args: string[]; pgpassword: string } {
  return {
    args: readFileSync(record, "utf8").split("\n").slice(0, -1),
    pgpassword: readFileSync(`${record}.pgpassword`, "utf8").trimEnd(),
  };
}

let restore: (() => void) | undefined;
beforeAll(() => {
  writeFileSync(
    join(standIn, "psql"),
    [
      "#!/bin/sh",
      `printf '%s\\n' "$@" >"$PSQL_PASSWORD_TEST_RECORD"`,
      `printf '%s\\n' "\${PGPASSWORD-unset}" >"$PSQL_PASSWORD_TEST_RECORD.pgpassword"`,
      "",
    ].join("\n")
  );
  chmodSync(join(standIn, "psql"), 0o755);
  restore = captureEnvironment([
    "PATH",
    "PGPASSWORD",
    "PSQL_PASSWORD_TEST_RECORD",
    "DATABASE_URL",
    "PLAYWRIGHT_BASE_URL",
    "PLAYWRIGHT_DATABASE_URL",
  ]);
  process.env.PATH = `${standIn}${delimiter}${process.env.PATH ?? ""}`;
  process.env.PGPASSWORD = "the-callers-own-not-a-secret";
  process.env.PSQL_PASSWORD_TEST_RECORD = record;
  delete process.env.PLAYWRIGHT_BASE_URL;
  delete process.env.PLAYWRIGHT_DATABASE_URL;
});

afterAll(() => {
  // The session psql of the session row is gone already; this lets the next `sql` of the process
  // start its own.
  resetSession();
  restore?.();
});

test("sqlAt hands psql the URL without its password, and the password in PGPASSWORD alone", async () => {
  await sqlAt(
    "postgres://tester:not-a-secret%40can%2Fary@127.0.0.1:5999/postgres?sslmode=disable",
    ["select 1"]
  );
  const { args, pgpassword } = handed();
  expect(args[0]).toBe("postgres://tester@127.0.0.1:5999/postgres?sslmode=disable");
  expect(args.filter((argument) => argument.includes("not-a-secret"))).toEqual([]);
  expect(pgpassword).toBe("not-a-secret@can/ary");
});

// The stand-in exits at once, so the session is lost and the call rejects; what it was started
// with is still on record.
test("the session psql of sql gets the URL without its password, and the password in PGPASSWORD alone", async () => {
  resetSession();
  process.env.DATABASE_URL =
    "postgres://tester@127.0.0.1:5999/session?sslmode=disable&password=not-a-secret%26x";
  await expect(sql("select 1")).rejects.toThrow();
  const { args, pgpassword } = handed();
  expect(args[0]).toBe("postgres://tester@127.0.0.1:5999/session?sslmode=disable");
  expect(args.filter((argument) => argument.includes("not-a-secret"))).toEqual([]);
  expect(pgpassword).toBe("not-a-secret&x");
});

test("a URL with no password reaches psql whole, and psql keeps the caller's own PGPASSWORD", async () => {
  for (const url of [
    "postgres://tester@127.0.0.1:5999/postgres?sslmode=disable",
    "postgres://127.0.0.1:5999/postgres",
  ]) {
    await sqlAt(url, ["select 1"]);
    expect(handed()).toEqual({
      args: [
        url,
        "-X",
        "-v",
        "ON_ERROR_STOP=1",
        "-v",
        "VERBOSITY=verbose",
        "-tA",
        "-c",
        "select 1",
      ],
      pgpassword: "the-callers-own-not-a-secret",
    });
  }
});

test("splitDatabaseUrl takes out every password= of the query string, the last one winning, and keeps every other parameter", () => {
  expect(
    splitDatabaseUrl(
      "postgresql://tester:not-a-secret-userinfo@[::1]:5432,db.test/dispatch?pass%77ord=not-a-secret-first&sslmode=disable&password=not-a-secret-q%26uery&application_name=a%3Db"
    )
  ).toEqual({
    url: "postgresql://tester@[::1]:5432,db.test/dispatch?sslmode=disable&application_name=a%3Db",
    password: "not-a-secret-q&uery",
  });
  expect(splitDatabaseUrl("postgres://tester:@127.0.0.1/postgres")).toEqual({
    url: "postgres://tester:@127.0.0.1/postgres",
    password: undefined,
  });
});

test("a password libpq cannot decode is refused before psql runs, naming the fault and not the URL", async () => {
  rmSync(record, { force: true });
  const call = sqlAt("postgres://tester:not-a-secret%zz@127.0.0.1:5999/postgres", ["select 1"]);
  await expect(call).rejects.toThrow(DatabaseUrlRefused);
  await expect(call).rejects.toThrow(
    "refused a Postgres URL: its password holds a % that starts no %XX escape, or %00 (the URL is not printed)"
  );
  expect(() => readFileSync(record)).toThrow();
});

// libpq decodes a password to its bytes and keeps them all, so one whose bytes start with a
// byte-order mark (EF BB BF) keeps it, and a query key that starts with one names no `password`.
test("a password whose bytes start with a byte-order mark keeps it, as libpq does", () => {
  expect(splitDatabaseUrl("postgres://tester:%EF%BB%BFnot-a-secret@127.0.0.1/postgres")).toEqual({
    url: "postgres://tester@127.0.0.1/postgres",
    password: "\uFEFFnot-a-secret",
  });
});

test("a query key %EF%BB%BFpassword is not the password, and stays in the URL", () => {
  const url = "postgres://tester@127.0.0.1/postgres?%EF%BB%BFpassword=not-a-secret&sslmode=disable";
  expect(splitDatabaseUrl(url)).toEqual({ url, password: undefined });
});

/** Runs `call` in bash with packages/envoy's shell split sourced and the functions `names` read out
 *  of `script` (this directory's run-broker.sh or acceptance/ci-check.sh), as the script defines
 *  them, with `url` in DATABASE_URL and database_url, so its psql is the stand-in's. */
function runScriptPsql(script: string, names: readonly string[], url: string, call: string): void {
  const e2e = join(import.meta.dir, "..");
  const definitions = names
    .map(
      (name) =>
        `eval "$(sed -n '/^${name}() {.*}$/p;/^${name}() {$/,/^}$/p' '${join(e2e, script)}')"`
    )
    .join("\n");
  const run = Bun.spawnSync(
    [
      "bash",
      "-c",
      `. '${join(e2e, "../../envoy/scripts/lib/pg-url.sh")}'\n${definitions}\n${call}`,
    ],
    { env: { ...process.env, DATABASE_URL: url, database_url: url } }
  );
  expect(run.stderr.toString()).toBe("");
  expect(run.exitCode).toBe(0);
}

test("run-broker.sh hands psql DATABASE_URL without its password, and the password in PGPASSWORD alone", () => {
  runScriptPsql(
    "run-broker.sh",
    ["psql_without_hostaddr", "broker_psql"],
    "postgres://tester:not-a-secret%21@127.0.0.1:5999/dispatch?sslmode=disable",
    "broker_psql -c 'select 1'"
  );
  const { args, pgpassword } = handed();
  expect(args[0]).toBe("postgres://tester@127.0.0.1:5999/dispatch?sslmode=disable");
  expect(args.filter((argument) => argument.includes("not-a-secret"))).toEqual([]);
  expect(pgpassword).toBe("not-a-secret!");
});

test("acceptance/ci-check.sh hands psql DATABASE_URL without its password, and the password in PGPASSWORD alone", () => {
  runScriptPsql(
    "acceptance/ci-check.sh",
    ["psql_without_hostaddr", "psql_quiet"],
    "postgres://tester@127.0.0.1:5999/dispatch?password=not-a-secret%21&sslmode=disable",
    "psql_quiet -c 'select 1'"
  );
  const { args, pgpassword } = handed();
  expect(args[0]).toBe("postgres://tester@127.0.0.1:5999/dispatch?sslmode=disable");
  expect(args.filter((argument) => argument.includes("not-a-secret"))).toEqual([]);
  expect(pgpassword).toBe("not-a-secret!");
});

/** The corpus the three splits share, packages/envoy/scripts/fixtures/pg-url.json, which
 *  scripts/search-measure/pg_test.py and packages/envoy/scripts/dev-broker.test.sh read too, so the
 *  three are held to one table; each case was checked against libpq's own PQconninfoParse. */
const corpus = (await Bun.file(
  new URL("../../../envoy/scripts/fixtures/pg-url.json", import.meta.url)
).json()) as {
  accepted: { url: string; password: string | null; bare: string }[];
  refused: { url: string; fault: string }[];
};

test("the shared corpus has accepted and refused cases", () => {
  expect([corpus.accepted.length > 0, corpus.refused.length > 0]).toEqual([true, true]);
});

test.each(corpus.accepted)("the shared corpus: splits $url as libpq does", ({
  url,
  password,
  bare,
}) => {
  expect(splitDatabaseUrl(url)).toEqual({ url: bare, password: password ?? undefined });
});

test.each(corpus.refused)("the shared corpus: refuses $url, naming $fault", ({ url, fault }) => {
  expect(() => splitDatabaseUrl(url)).toThrow(
    `refused a Postgres URL: ${fault} (the URL is not printed)`
  );
});

test.skipIf(process.platform !== "linux")(
  "run-broker.sh execs its broker with the database URL in its environment and in no process's arguments",
  () => {
    const e2e = join(import.meta.dir, "..");
    const work = join(standIn, "broker-work");
    mkdirSync(join(work, "bin"), { recursive: true });
    // A stand-in broker that writes its own arguments and the database URL it was given, and an
    // env first on PATH that records its arguments before it runs, as env would carry settings.
    writeFileSync(
      join(work, "broker"),
      [
        "#!/bin/sh",
        `tr '\\0' ' ' </proc/$$/cmdline >'${work}/cmdline'`,
        `printf '%s' "$BROKER_DATABASE_URL" >'${work}/database-url'`,
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    writeFileSync(
      join(work, "bin", "env"),
      `#!/bin/sh\nprintf '%s\\n' "$*" >>'${work}/env-args'\nexec ${Bun.which("env")} "$@"\n`,
      { mode: 0o755 }
    );
    const url = "postgres://tester:not-a-secret%40broker@127.0.0.1:5999/dispatch_broker";
    const run = Bun.spawnSync(
      [
        "bash",
        "-c",
        `eval "$(sed -n '/^exec_broker() {$/,/^}$/p' '${join(e2e, "run-broker.sh")}')"\nexec_broker`,
      ],
      {
        env: {
          PATH: `${join(work, "bin")}${delimiter}${process.env.PATH ?? ""}`,
          broker_database_url: url,
          work,
        },
      }
    );
    expect(run.stderr.toString()).toBe("");
    const envArgs = existsSync(join(work, "env-args"))
      ? readFileSync(join(work, "env-args"), "utf8")
      : "";
    expect({
      envArgs: envArgs.includes("not-a-secret"),
      cmdline: readFileSync(join(work, "cmdline"), "utf8").includes("not-a-secret"),
      databaseUrl: readFileSync(join(work, "database-url"), "utf8"),
    }).toEqual({ envArgs: false, cmdline: false, databaseUrl: url });
  }
);

test.skipIf(process.platform !== "linux" || Bun.which("openssl") === null)(
  "run-server.sh hands its server DATABASE_URL, the App key and the sign-in key in its environment and in no process's arguments",
  () => {
    const e2e = join(import.meta.dir, "..");
    const work = join(standIn, "server-work");
    mkdirSync(join(work, "bin"), { recursive: true });
    // A python3 and an env first on PATH that record their arguments before they run, as
    // held-port.py and env would carry a setting named to them, and a stand-in server that
    // writes its own arguments and the settings it was given.
    for (const tool of ["python3", "env"]) {
      writeFileSync(
        join(work, "bin", tool),
        `#!/bin/sh\nprintf '%s\\n' "${tool} $*" >>'${work}/argv'\nexec ${Bun.which(tool)} "$@"\n`,
        { mode: 0o755 }
      );
    }
    writeFileSync(
      join(work, "server"),
      [
        "#!/bin/sh",
        `tr '\\0' ' ' </proc/$$/cmdline >'${work}/server-cmdline'`,
        `printf '%s\\n%s\\n%s\\n' "$DATABASE_URL" "$DISPATCH_DEV_SIGNIN_SHARED_KEY" "\${#DISPATCH_APP_PEM_B64}" >'${work}/server-env'`,
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    const url = "postgres://tester:not-a-secret%40server@127.0.0.1:5999/dispatch_e2e";
    const sharedKey = "not-a-secret-shared-key";
    const run = Bun.spawnSync(["bash", join(e2e, "run-server.sh")], {
      env: {
        PATH: `${join(work, "bin")}${delimiter}${process.env.PATH ?? ""}`,
        DATABASE_URL: url,
        DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY: sharedKey,
        DISPATCH_E2E_SERVER_BIN: join(work, "server"),
        FAKE_BROKER_PORT: "9024",
        FAKE_ENVOY_PORT: "9021",
        FAKE_GITHUB_PORT: "9022",
      },
    });
    expect(run.exitCode, run.stderr.toString()).toBe(0);
    const argv = readFileSync(join(work, "argv"), "utf8");
    const [databaseUrl, shared, pemLength] = readFileSync(join(work, "server-env"), "utf8").split(
      "\n"
    );
    expect({
      recorded: argv.includes("python3 ") && argv.includes("env "),
      argvPassword: argv.includes("not-a-secret"),
      argvKey: argv.includes("PRIVATE KEY"),
      cmdline: readFileSync(join(work, "server-cmdline"), "utf8").includes("not-a-secret"),
      databaseUrl,
      shared,
      pemGiven: Number(pemLength) > 0,
    }).toEqual({
      recorded: true,
      argvPassword: false,
      argvKey: false,
      cmdline: false,
      databaseUrl: url,
      shared: sharedKey,
      pemGiven: true,
    });
  }
);

/** What `dispatch:local` prints once its instance is up, for the database `databaseUrl`. */
function handOver(databaseUrl: string, supplied: boolean): string {
  return handOverMessage({
    baseUrl: "http://127.0.0.1:9030",
    broadcastId: "broadcast-1",
    plannerSessionId: "planner-1",
    fakeEnvoy: "http://127.0.0.1:9031",
    databaseUrl,
    supplied,
    ports: { DISPATCH_E2E_PORT: "9030", FAKE_ENVOY_PORT: "9031" },
    env: {},
  });
}

/** The DATABASE_URL the hand-over's command gives its run, as bash reads that command's words. */
function pastedDatabaseUrl(message: string): string {
  const command = message
    .split("\n")
    .find((line) => line.startsWith("    DISPATCH_E2E_REUSE_SERVERS=1 "));
  if (command === undefined) throw new Error(`the hand-over holds no command:\n${message}`);
  const prefix = command.trim().replace(/ bunx playwright test .*$/, "");
  const read = Bun.spawnSync(["bash", "-c", `${prefix} printenv DATABASE_URL`]);
  if (read.exitCode !== 0) throw new Error(`bash read no DATABASE_URL: ${read.stderr.toString()}`);
  return read.stdout.toString().trimEnd();
}

// The command's run reuses the instance's servers (DISPATCH_E2E_REUSE_SERVERS=1), so its one
// reader of DATABASE_URL is e2e/psql.ts; the session psql of `sql` stands in for it here.
test("dispatch:local's hand-over prints no password a person gave in DATABASE_URL, and its command still reaches psql with that password in PGPASSWORD", async () => {
  for (const [given, bare, password] of [
    [
      "postgres://tester:not-a-secret%40hand@127.0.0.1:5999/dispatch_local?sslmode=disable",
      "postgres://tester@127.0.0.1:5999/dispatch_local?sslmode=disable",
      "not-a-secret@hand",
    ],
    [
      "postgres://tester@127.0.0.1:5999/dispatch_local?password=not-a-secret-query&sslmode=disable",
      "postgres://tester@127.0.0.1:5999/dispatch_local?sslmode=disable",
      "not-a-secret-query",
    ],
  ] as const) {
    const message = handOver(given, true);
    const pasted = pastedDatabaseUrl(message);
    resetSession();
    process.env.DATABASE_URL = pasted;
    process.env.PGPASSWORD = password;
    try {
      await expect(sql("select 1")).rejects.toThrow();
    } finally {
      process.env.PGPASSWORD = "the-callers-own-not-a-secret";
    }
    const { args, pgpassword } = handed();
    expect({
      printed: message.split("\n").filter((line) => line.includes("not-a-secret")),
      pasted,
      saysPgpassword: message.includes("exported as PGPASSWORD"),
      psqlUrl: args[0],
      pgpassword,
    }).toEqual({
      printed: [],
      pasted: bare,
      saysPgpassword: true,
      psqlUrl: bare,
      pgpassword: password,
    });
  }
});

test("dispatch:local's hand-over prints the dev container's URL and a URL with no password whole, and asks for no PGPASSWORD", () => {
  // The dev container's URL is one the command makes itself (`supplied` false), so it prints whole
  // whatever its password: the stand-in here takes the place of the dev container's literal.
  for (const [url, supplied] of [
    ["postgres://postgres:not-a-secret-dev@127.0.0.1:55432/dispatch_local?sslmode=disable", false],
    ["postgres://tester@127.0.0.1:5999/dispatch_local?sslmode=disable", true],
  ] as const) {
    const message = handOver(url, supplied);
    expect({
      database: message.includes(`  Database (truncated and reseeded on every start): ${url}\n`),
      pasted: pastedDatabaseUrl(message),
      saysPgpassword: message.includes("PGPASSWORD"),
    }).toEqual({ database: true, pasted: url, saysPgpassword: false });
  }
});
