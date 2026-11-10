import { expect, test } from "bun:test";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

import { startStacks } from "../harness-pool";
import {
  harnessPorts,
  refuseStackConflicts,
  stackConflicts,
  stackIndex,
  stacksVariable,
  stackValue,
  withoutHarnessPort,
} from "../harness-ports";

const psqlModule = fileURLToPath(new URL("../psql.ts", import.meta.url));
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

// Every child this file starts reads this home rather than the caller's, so no cache,
// configuration or credential of the developer's own can change what a case here proves.
const home = temporaryDirectory("harness-stacks-home");

/** Two stacks as `e2e/global-setup.ts` writes them: one record per stack, each holding that
 *  stack's own variables. */
const manifest = JSON.stringify([
  { DATABASE_URL: "postgres://stack-0", FAKE_ENVOY_PORT: "9100" },
  { DATABASE_URL: "postgres://stack-1", FAKE_ENVOY_PORT: "9101", PLAIN_HTTP_PORT: "9301" },
]);

async function run(argv: string[], env: Record<string, string>) {
  const child = Bun.spawn(argv, {
    cwd: dispatchPackage,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      // The child writes under its temporary directory, which on a machine whose TMPDIR names
      // the only writable volume is not /tmp.
      TMPDIR: tmpdir(),
      ...env,
    },
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new Response(child.stderr).text(),
    new Response(child.stdout).text(),
  ]);
  return { code, stderr, stdout };
}

test("a worker reads the manifest record its own index selects", () => {
  // Worker 1 is the manifest's second record, under every name that record holds.
  const worker = { [stacksVariable]: manifest, TEST_PARALLEL_INDEX: "1" };
  expect(stackValue(worker, "FAKE_ENVOY_PORT")).toBe("9101");
  expect(stackValue(worker, "DATABASE_URL")).toBe("postgres://stack-1");
  // A process with no index of its own is stack 0, which is the config's own evaluation,
  // `globalSetup` itself and every listener the pool starts.
  expect(stackValue({ [stacksVariable]: manifest }, "FAKE_ENVOY_PORT")).toBe("9100");
  // A plain variable with no manifest at all resolves itself: a run against servers a person
  // started, a deployed run, and every process the pool hands a stack's values to.
  expect(stackValue({ FAKE_ENVOY_PORT: "9100" }, "FAKE_ENVOY_PORT")).toBe("9100");
  // The manifest wins over the plain name where its record holds the variable, and the plain
  // name answers where the record does not.
  const both = { ...worker, DISPATCH_E2E_PORT: "9200", FAKE_ENVOY_PORT: "9999" };
  expect(stackValue(both, "FAKE_ENVOY_PORT")).toBe("9101");
  expect(stackValue(both, "DISPATCH_E2E_PORT")).toBe("9200");
  // An index the manifest holds no record for falls through to the plain name rather than
  // refusing: those names are stack 0's.
  expect(stackValue({ ...both, TEST_PARALLEL_INDEX: "7" }, "FAKE_ENVOY_PORT")).toBe("9999");
});

test("a malformed manifest is refused by name, whatever shape it has", () => {
  for (const given of ["{", "{}", '"one"', "[1]", '[{"FAKE_ENVOY_PORT":9100}]', "[[]]"]) {
    expect(() => stackValue({ [stacksVariable]: given }, "FAKE_ENVOY_PORT")).toThrow(
      `${stacksVariable} must hold this run's stacks as JSON`
    );
  }
  // An empty value is an unset one: nothing started a pool, so the plain names answer.
  expect(stackValue({ [stacksVariable]: "", FAKE_ENVOY_PORT: "9100" }, "FAKE_ENVOY_PORT")).toBe(
    "9100"
  );
});

test("the stack index is Playwright's own, validated", () => {
  expect(stackIndex({})).toBe(0);
  expect(stackIndex({ TEST_PARALLEL_INDEX: "" })).toBe(0);
  expect(stackIndex({ TEST_PARALLEL_INDEX: "3" })).toBe(3);
  for (const value of ["-1", "one", " 2", "2.5", "01"]) {
    expect(() => stackIndex({ TEST_PARALLEL_INDEX: value })).toThrow(
      `TEST_PARALLEL_INDEX must be the stack's index, a number from 0 up, not ${JSON.stringify(value)}`
    );
  }
});

test("a spec that starts a listener of its own clears the port under both of its names", () => {
  const env = {
    [stacksVariable]: manifest,
    FAKE_ENVOY_PORT: "9100",
    PLAIN_HTTP_PORT: "9300",
    TEST_PARALLEL_INDEX: "1",
  };
  // Both names gone, so the child binds port 0 rather than the port the harness listener holds,
  // and the other listeners' ports reach it untouched.
  const cleared = withoutHarnessPort("fakeEnvoy", env);
  expect(harnessPorts(cleared).fakeEnvoy.port).toBe(0);
  expect(harnessPorts(cleared).plainHttp.port).toBe(9301);
  expect(cleared.TEST_PARALLEL_INDEX).toBe("1");
  // Only this process's own record is rewritten: the child is still part of this run, and the
  // other stacks' servers are still up.
  expect(stackValue({ ...cleared, TEST_PARALLEL_INDEX: "0" }, "FAKE_ENVOY_PORT")).toBe("9100");
  // A run of one stack names the port once, under the plain name, and stack 0 is what a process
  // with no index resolves.
  const one = { FAKE_ENVOY_PORT: "9100" };
  expect(harnessPorts(withoutHarnessPort("fakeEnvoy", one)).fakeEnvoy.port).toBe(0);
});

test("harnessPorts resolves the stack's ports and keeps both of its refusals", () => {
  const ports = JSON.stringify([
    { FAKE_ENVOY_PORT: "9100" },
    { DISPATCH_E2E_PORT: "9201", FAKE_ENVOY_PORT: "9101" },
  ]);
  const worker = { [stacksVariable]: ports, DISPATCH_E2E_PORT: "9200", TEST_PARALLEL_INDEX: "1" };
  expect(harnessPorts(worker).fakeEnvoy.port).toBe(9101);
  expect(harnessPorts(worker).dispatch.port).toBe(9201);
  // Stack 1 names no plain-HTTP port, so it falls back to the plain name, unset here.
  expect(harnessPorts(worker).plainHttp.port).toBe(0);
  // Stack 0 names no Dispatch port, so it reads the one DISPATCH_E2E_PORT names.
  expect(harnessPorts({ ...worker, TEST_PARALLEL_INDEX: "0" }).dispatch.port).toBe(9200);
  const malformed = JSON.stringify([{}, { FAKE_ENVOY_PORT: "0x2249" }]);
  expect(() => harnessPorts({ ...worker, [stacksVariable]: malformed })).toThrow(
    "FAKE_ENVOY_PORT must be a port number"
  );
  const duplicated = JSON.stringify([{}, { DISPATCH_E2E_PORT: "9101", FAKE_ENVOY_PORT: "9101" }]);
  expect(() => harnessPorts({ ...worker, [stacksVariable]: duplicated })).toThrow(
    "DISPATCH_E2E_PORT and FAKE_ENVOY_PORT name port 9101"
  );
});

// What a run of more than one stack refuses before it starts anything, and why the list is
// exactly these. Each variable names a thing ONE stack owns, so a run that honoured it would
// hand every stack the one database, the one set of servers or the one port - and the rows
// would then delete each other's fixtures, because e2e/test.ts truncates that database before
// every row, and fail as though the product were broken. `DISPATCH_E2E_REUSE_SERVERS` is the
// one this suite lost and had to get back: `globalSetup` returns on it, so without the refusal
// `--workers 4 DISPATCH_E2E_REUSE_SERVERS=1` put four workers on one server in silence.
test("a run of more than one stack refuses every variable one stack owns, by name", () => {
  const refused: Record<string, string> = {
    DATABASE_URL: "postgres://localhost/dispatch",
    DISPATCH_E2E_PROCESSES: "2",
    DISPATCH_E2E_REUSE_SERVERS: "1",
    PLAYWRIGHT_BASE_URL: "https://dispatch.example",
    // One harness port, which is refused for its own reason: a port is one listener's.
    FAKE_ENVOY_PORT: "9100",
  };
  for (const [variable, value] of Object.entries(refused)) {
    expect(stackConflicts({ [variable]: value })).toEqual([variable]);
    expect(() => refuseStackConflicts(2, { [variable]: value })).toThrow(
      new RegExp(`2 Playwright workers run 2 stacks.*${variable} must not be set`, "s")
    );
    // One worker is one stack, and one stack is what every one of these belongs to.
    expect(() => refuseStackConflicts(1, { [variable]: value })).not.toThrow();
  }
  // `DISPATCH_E2E_PROCESSES=1` is what one stack runs, so it is no conflict.
  expect(stackConflicts({ DISPATCH_E2E_PROCESSES: "1" })).toEqual([]);
});

test("a run of more than one stack that sets none of them is not refused", () => {
  expect(stackConflicts({ HOME: home, TMPDIR: tmpdir() })).toEqual([]);
  expect(() => refuseStackConflicts(2, { HOME: home, TMPDIR: tmpdir() })).not.toThrow();
});

test("a run of more than one stack refuses to start without a Postgres provider", async () => {
  // Each stack runs a Postgres of its own, because e2e/seed.ts truncates its database before
  // every row. The refusal comes before a server starts, and it leaves nothing behind: the run
  // directory it made is removed with everything else the failed start held.
  await expect(startStacks(2, { env: {} })).rejects.toThrow(
    "2 Playwright workers run 2 stacks, each with a Postgres of its own"
  );
});

// The database a stack reads is the psql it starts, so the case that proves which one it picked
// needs psql and a live database to answer; without either it would prove nothing and is
// skipped, as e2e/__tests__/psql-session.test.ts's cases are, and its two halves are composed
// into one line for the reason that file gives. The case runs `select 1` and writes nothing, so
// it runs on the server's own database rather than one of its own.
const live = (process.env.LEGION_TEST_PG_DSN ?? "").trim();
const missingForDatabase = [
  ...(hostTool("psql") === null ? ["psql on this host's PATH"] : []),
  ...(live === "" ? ["LEGION_TEST_PG_DSN, naming a server this run may read"] : []),
];
const withoutDatabase = missingForDatabase.length > 0;
if (withoutDatabase) {
  console.warn(
    `Skipped the test in ${import.meta.path} that needs a database. This run does not have: ` +
      `${missingForDatabase.join("; ")}. Supply what is missing and run the tests again.`
  );
}

test.skipIf(withoutDatabase)("psql reads its own stack's database", async () => {
  // A process of its own per case, which imports e2e/psql.ts in the environment under test: it
  // resolves the database once, when it starts its psql.
  const resolve = [
    "bun",
    "-e",
    `await (await import(${JSON.stringify(psqlModule)})).sql("select 1")`,
  ];
  const dead = "postgres://postgres@127.0.0.1:1/never-opened?sslmode=disable";
  const stacks = JSON.stringify([{ DATABASE_URL: dead }, { DATABASE_URL: live }]);
  // Stack 1's database answers; the plain name, which stack 1 must not read, is a dead port.
  const stack = await run(resolve, {
    DATABASE_URL: dead,
    [stacksVariable]: stacks,
    TEST_PARALLEL_INDEX: "1",
  });
  expect(`${stack.stderr}${stack.stdout}`).toBe("");
  expect(stack.code).toBe(0);
  // The same environment without the worker's index is stack 0, whose record names the dead one.
  const plain = await run(resolve, { DATABASE_URL: live, [stacksVariable]: stacks });
  expect(plain.code).not.toBe(0);
});

test("psql names both variables when neither its stack nor the plain name has a database", async () => {
  const refused = await run(
    ["bun", "-e", `await (await import(${JSON.stringify(psqlModule)})).sql("select 1")`],
    { [stacksVariable]: JSON.stringify([{}, {}]), TEST_PARALLEL_INDEX: "1" }
  );
  expect(refused.code).not.toBe(0);
  expect(`${refused.stderr}${refused.stdout}`).toContain(
    "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
  );
});
