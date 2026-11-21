import { beforeAll, describe, expect, test } from "bun:test";
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { until } from "@legion/test-fixtures/wait";

// The key command install-model-gateway.sh writes, against a fake hawk-token first on PATH. The
// fake appends its pid to FAKE_MINT_LOG (one line per mint), sleeps FAKE_MINT_SLEEP seconds, then
// by FAKE_MINT_MODE prints a JWT that expires in an hour (ok), fails the way the devbox wrapper
// does when its 9000 ms budget runs out (budget), fails at once on the login (refused), is
// killed by a signal (killed), or never answers (hang): it waits until a signal stops it, and
// appends the signal's name and its pid to FAKE_MINT_LOG.stopped.
const lib = import.meta.dir;
const dir = temporaryDirectory("install-model-gateway-test");
const bin = join(dir, "bin");
const operatorHome = join(dir, "operator-home");
beforeAll(() => {
  mkdirSync(bin);
  mkdirSync(operatorHome);
  writeFileSync(
    join(bin, "hawk-token"),
    `#!/usr/bin/env bash
printf '%s\\n' "$$" >>"$FAKE_MINT_LOG"
sleep "\${FAKE_MINT_SLEEP:-0}"
case "\${FAKE_MINT_MODE:-ok}" in
ok)
  payload=$(printf '{"exp":%s}' "$(($(date +%s) + 3600))" | base64 -w0 | tr '+/' '-_' | tr -d '=')
  printf 'eyJhbGciOiJub25lIn0.%s.c2ln\\n' "$payload"
  ;;
budget)
  echo "hawk-token: mint produced no token after 1 attempt(s) in 9021 ms of a 9000 ms budget; last stderr follows" >&2
  exit 1
  ;;
refused)
  echo "hawk-token: mint produced no token after 2 attempt(s) in 812 ms of a 9000 ms budget; last stderr follows" >&2
  echo "error: no usable hawk login: run hawk login" >&2
  exit 1
  ;;
killed) kill -KILL $$ ;;
hang)
  trap 'printf "TERM %s\\n" "$$" >>"$FAKE_MINT_LOG.stopped"; exit 143' TERM
  sleep infinity >/dev/null &
  wait "$!"
  ;;
esac
`,
    { mode: 0o755 }
  );
});

interface Fake {
  mode?: "ok" | "budget" | "refused" | "killed" | "hang";
  sleep?: number;
  // The MODEL_GATEWAY_CALLS_FILE a harness names in one agent's environment.
  callsFile?: string;
  // A Legion pane's identity, as the Go tmux runtime sets it on the pane's environment.
  role?: string;
  generation?: string;
}
let installs = 0;
function env(mints: string, fake: Fake = {}) {
  return {
    PATH: `${bin}:${process.env.PATH}`,
    HOME: operatorHome,
    DBUS_SESSION_BUS_ADDRESS: "unix:path=/nonexistent/bus",
    LEGION_E2E_MODEL_GATEWAY_URL: "https://gateway.internal.example/anthropic",
    FAKE_MINT_LOG: mints,
    FAKE_MINT_MODE: fake.mode ?? "ok",
    FAKE_MINT_SLEEP: String(fake.sleep ?? 0),
    ...(fake.callsFile === undefined ? {} : { MODEL_GATEWAY_CALLS_FILE: fake.callsFile }),
    ...(fake.role === undefined ? {} : { LEGION_ROLE: fake.role }),
    ...(fake.generation === undefined ? {} : { LEGION_GENERATION: fake.generation }),
  };
}

// install writes a fresh profile's route, its preflight mint (with the given fake) included, and
// then removes the key that mint kept, so the next call finds the cache cold (the state the kept
// key's expiry leaves).
function install(fake: Fake = {}) {
  const run = join(dir, `install-${++installs}`);
  const home = join(run, "omp-home");
  mkdirSync(join(home, ".omp"), { recursive: true });
  const mints = join(run, "mints");
  const dest = join(run, "model-gateway");
  const cache = join(run, "model-gateway-cache");
  const result = Bun.spawnSync(
    [
      "bash",
      join(lib, "install-model-gateway.sh"),
      "--profile",
      "legion-e2e-test",
      "--home",
      home,
      "--dest",
      dest,
      "--cache-dir",
      cache,
    ],
    { env: env(mints, fake) }
  );
  expect(result.exitCode).toBe(0);
  // The key the preflight's mint kept, which the fake prints only once its sleep is over.
  expect(existsSync(join(cache, "hawk-token.key")), "the preflight kept no key").toBe(true);
  rmSync(join(cache, "hawk-token.key"));
  writeFileSync(mints, "");
  return { run, dest, cache, mints, keyCommand: result.stdout.toString().trim() };
}

// call runs the key command as Oh My Pi does, in a pane's own working directory.
async function call(keyCommand: string, cwd: string, mints: string, fake: Fake = {}) {
  mkdirSync(cwd, { recursive: true });
  const child = Bun.spawn(["/bin/sh", "-c", `exec ${keyCommand}`], {
    cwd,
    env: env(mints, fake),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stdout: stdout.trim(), stderr: stderr.trim() };
}

function unserved(...args: string[]) {
  const result = Bun.spawnSync(["bash", join(lib, "model-gateway-unserved.sh"), ...args], {
    env: { PATH: process.env.PATH ?? "" },
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

const mintCount = (mints: string) => readFileSync(mints, "utf8").split("\n").filter(Boolean).length;
// The record's own time format, UTC to the second.
const now = () => new Date().toISOString().replace(/\.\d+Z$/, "Z");

// How long Oh My Pi lets a `!command` config value run: `executeCommand` in
// packages/coding-agent/src/config/resolve-config-value.ts of github.com/cybermonklabs/oh-my-pi
// runs it as `runShellCommand(command, 10_000, cwd)` (line 76 at v18.18.6-sami.20261117-013345,
// the release .omp-pin names), whose process tree `ptree.exec` stops at that timeout.
const ohMyPiCommandMs = 10_000;

// The bound every call but the preflight gives up at: the `deadline_ms` the key command sets.
function callBoundMs(keyCommand: string): number {
  const bound = /^deadline_ms=(\d+)$/m.exec(readFileSync(keyCommand, "utf8"))?.[1];
  if (bound === undefined) throw new Error(`${keyCommand} sets no deadline_ms`);
  return Number(bound);
}

describe("the model gateway key command", () => {
  test("mints once for a wave of callers that finds the kept key expired, and serves them all", async () => {
    const { run, mints, keyCommand } = install();
    const calls = await Promise.all(
      [1, 2, 3, 4, 5].map((n) => call(keyCommand, join(run, `pane-${n}`), mints, { sleep: 1 }))
    );

    expect(mintCount(mints)).toBe(1);
    for (const c of calls) expect(c.code).toBe(0);
    expect(new Set(calls.map((c) => c.stdout)).size).toBe(1);
    expect(calls[0].stdout).toMatch(/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
  });

  test("tells an agent that starved from one whose login was refused or whose mint was killed, in its own file", async () => {
    const { run, mints, keyCommand } = install();
    const calls = (name: string) => join(run, name, "model-gateway-calls");
    const outcome = async (name: string, fake: Fake) => {
      const c = await call(keyCommand, join(run, name, "cwd"), mints, {
        ...fake,
        callsFile: calls(name),
      });
      return [c.code, c.stdout];
    };

    // What Oh My Pi sees is the same for each, which is why the run itself cannot tell them apart.
    expect(await outcome("starved", { mode: "budget" })).toEqual([1, ""]);
    expect(await outcome("refused", { mode: "refused" })).toEqual([1, ""]);
    expect(await outcome("killed", { mode: "killed" })).toEqual([1, ""]);
    // Each agent's own file does.
    const s = unserved("--record", calls("starved"));
    expect(s.code).toBe(75);
    expect(s.stdout).toContain(`in ${join(run, "starved", "cwd")} (pid `);
    expect(s.stdout).toContain("): timeout: ");
    expect(s.stdout).toContain("9021 ms of a 9000 ms budget");
    const r = unserved("--record", calls("refused"));
    expect(r.code).toBe(77);
    expect(r.stdout).toContain(": failed: ");
    expect(r.stdout).toContain("no usable hawk login");
    // A signal is no login problem: it is a starve, which a rerun can fix.
    const k = unserved("--record", calls("killed"));
    expect(k.code).toBe(75);
    expect(k.stdout).toContain(": killed: ");
    // An agent never called, and a path whose directory does not exist, which is a harness mistake.
    expect(unserved("--record", calls("never"))).toMatchObject({ code: 2 });
    mkdirSync(join(run, "never"));
    expect(unserved("--record", calls("never"))).toMatchObject({ code: 0, stdout: "" });
  });

  test("judges a run by its agent's last call, so a starve Oh My Pi retried past counts as served", async () => {
    const { run, mints, keyCommand } = install();
    const file = join(run, "pane", "model-gateway-calls");
    const pane = join(run, "pane", "cwd");
    await call(keyCommand, pane, mints, { mode: "budget", callsFile: file });
    expect(unserved("--record", file).code).toBe(75);

    // Oh My Pi retries the key command, and this time it mints.
    expect((await call(keyCommand, pane, mints, { callsFile: file })).code).toBe(0);
    expect(unserved("--record", file)).toMatchObject({ code: 0, stdout: "" });
  });

  test("tells apart the agents of one issue, which share its workspace, by their Legion pane", async () => {
    const { run, dest, mints, keyCommand } = install();
    const since = now();
    const workspace = join(run, "issue-workspace");
    // The tester's mint fails, and the architect's call in the same directory is served after it.
    await call(keyCommand, workspace, mints, { mode: "refused", role: "tester", generation: "1" });
    await call(keyCommand, workspace, mints, { role: "architect", generation: "2" });

    const notes = unserved("--notes", dest, since, "tester-handoff");
    expect(notes.code).toBe(0);
    const listed = notes.stdout.split("\n").filter((line) => line.startsWith("  "));
    expect(listed).toHaveLength(1);
    expect(listed[0]).toContain(` tester/1 in ${workspace} (pid `);
    expect(listed[0]).toContain(
      ": failed: hawk-token exited 1 without a key: error: no usable hawk login"
    );
    expect(listed[0]).not.toContain("served again");
    const record = readFileSync(join(dest, "hawk-token.calls"), "utf8");
    expect(record).toContain("\ttester/1\tfailed\t");
    expect(record).toContain("\tarchitect/2\tserved\t");

    // `legion controller start` sets LEGION_ROLE=controller and no generation; a harness sets
    // neither.
    await call(keyCommand, join(run, "controller"), mints, { role: "controller" });
    await call(keyCommand, join(run, "harness"), mints);
    const fields = readFileSync(join(dest, "hawk-token.calls"), "utf8")
      .split("\n")
      .filter(Boolean)
      .map((l) => l.split("\t"));
    expect(fields.every((f) => f.length === 6 && f.every(Boolean))).toBe(true);
    expect(fields.at(-2)?.[3]).toBe("controller");
    expect(fields.at(-1)?.[3]).toBe("-");
  });

  test("lists for a failed check each agent still without a key, and each that recovered from a starve within 30 s of the check's start", () => {
    // The rules are the reader's over the record's own lines, so this writes them at chosen times.
    const dest = join(dir, "notes-window");
    mkdirSync(dest);
    const since = "2026-09-30T12:00:00Z";
    const line = (at: string, agent: string, outcome: string) =>
      `${at}\t42\t/ws\t${agent}\t${outcome}\t${outcome === "served" ? "the kept key" : "why"}\n`;
    writeFileSync(
      join(dest, "hawk-token.calls"),
      [
        line("2026-09-30T11:50:00Z", "implementer/1", "timeout"), // still without, long before
        line("2026-09-30T11:50:00Z", "reviewer/1", "timeout"),
        line("2026-09-30T11:50:10Z", "reviewer/1", "served"), // recovered before the window
        line("2026-09-30T11:59:50Z", "planner/1", "killed"), // inside the 30 s window
        line("2026-09-30T12:00:02Z", "planner/1", "served"),
        line("2026-09-30T11:59:50Z", "merger/1", "killed"),
        line("2026-09-30T12:00:02Z", "merger/1", "served"),
        // A second starve after that service: its own first service is the one that counts.
        line("2026-09-30T12:00:10Z", "merger/1", "timeout"),
        line("2026-09-30T12:00:15Z", "merger/1", "served"),
        line("2026-09-30T12:00:20Z", "merger/1", "served"),
        line("2026-09-30T12:00:05Z", "tester/1", "timeout"), // still without, during the check
        line("2026-09-30T12:00:06Z", "architect/1", "served"),
      ].join("")
    );

    const notes = unserved("--notes", dest, since, "merge");
    expect(notes.code).toBe(0);
    expect(notes.stdout).toStartWith(
      "model-gateway-unserved: 4 agent(s) may have failed check merge for want of a model key: 2 still without one on their last call, and 2 served again after a starve from 2026-09-30T11:59:30Z on"
    );
    const listed = notes.stdout.split("\n").filter((l) => l.startsWith("  "));
    expect(listed).toEqual([
      "  2026-09-30T11:50:00Z implementer/1 in /ws (pid 42): timeout: why (before check merge began)",
      "  2026-09-30T11:59:50Z planner/1 in /ws (pid 42): killed: why (before check merge began); served again at 2026-09-30T12:00:02Z",
      "  2026-09-30T12:00:05Z tester/1 in /ws (pid 42): timeout: why",
      "  2026-09-30T12:00:10Z merger/1 in /ws (pid 42): timeout: why; served again at 2026-09-30T12:00:15Z",
    ]);
    // No record yet, a malformed time, and a line of the old five-field shape.
    expect(unserved("--notes", join(dir, "not-installed"), since, "setup")).toMatchObject({
      code: 0,
      stdout: "",
    });
    expect(unserved("--notes", dest, "yesterday", "setup").code).toBe(2);
    writeFileSync(join(dest, "hawk-token.calls"), "2026-09-30T12:00:05Z\t42\t/ws\ttimeout\twhy\n");
    expect(unserved("--notes", dest, since, "merge").code).toBe(1);
  });

  test("stops a mint that never answers at the call's bound, and gives up on the wait behind it", async () => {
    const { run, dest, mints, keyCommand } = install();
    const bound = callBoundMs(keyCommand);
    const since = now();
    // The bound under test is the key command's own clock, in another process, which no fake timer
    // reaches, so the row reads what each side does at it rather than when. The stand-in never
    // answers, so only the key command ends its mint: the stand-in sees the TERM of the `timeout`
    // the key command runs it under. The second caller starts once the first call's mint has
    // started, under the lock: it waits behind that mint, and takes the lock the first releases
    // when its mint is stopped with too little of its own bound left to mint, or none.
    const first = call(keyCommand, join(run, "pane-1"), mints, { mode: "hang" });
    await until("the first call's mint to start", () => mintCount(mints) === 1);
    const minter = readFileSync(mints, "utf8").trim();
    const second = call(keyCommand, join(run, "pane-2"), mints, { mode: "hang" });
    const stopped = `${mints}.stopped`;
    await until(
      `the key command to stop the mint that never answers (pid ${minter})`,
      () => existsSync(stopped) && readFileSync(stopped, "utf8").includes(`TERM ${minter}\n`)
    );
    const [one, two] = await Promise.all([first, second]);

    expect(mintCount(mints)).toBe(1);
    // The first call ends with the timeout's own status, 124, and names the bound it stopped at.
    expect([one.code, one.stdout, one.stderr]).toEqual([
      1,
      "",
      `hawk-token key command: no key (timeout): the mint ran past the call's ${bound} ms and was stopped (exit 124)`,
    ]);
    expect([two.code, two.stdout]).toEqual([1, ""]);
    expect(two.stderr).toMatch(
      new RegExp(
        `^hawk-token key command: no key \\(timeout\\): waited \\d+ ms for another call's mint\\b.* the call's ${bound} ms\\b`
      )
    );
    const outcomes = new Map(
      readFileSync(join(dest, "hawk-token.calls"), "utf8")
        .split("\n")
        .filter(Boolean)
        .map((l) => l.split("\t"))
        .map((fields) => [fields[2], fields.slice(4).join("\t")] as const)
    );
    expect(outcomes.get(join(run, "pane-1"))).toStartWith(
      `timeout\tthe mint ran past the call's ${bound} ms and was stopped`
    );
    expect(outcomes.get(join(run, "pane-2"))).toMatch(
      new RegExp(`^timeout\\twaited \\d+ ms for another call's mint\\b.* the call's ${bound} ms\\b`)
    );
    const notes = unserved("--notes", dest, since, "outlast").stdout;
    expect(notes).toContain(`: timeout: the mint ran past the call's ${bound} ms`);
    expect(notes).toContain("for another call's mint");
    // How long each call could take is the key command's own record of each timeout it armed and
    // the time it had left when it armed it, read the moment it did (R4: the bound the program
    // chose, across the process boundary, not timed here): every timeout is at most the time the
    // call had left of its bound, so a call that armed one past its bound fails here, whenever
    // the stand-in's TERM came. Both calls armed their lock wait, and the first its mint.
    const armed = [
      ...readFileSync(join(dest, "hawk-token.log"), "utf8").matchAll(
        /armed the (lock wait|mint) for (\d+)\.(\d{3}) s with (-?\d+) ms of the call's (\d+) ms left/g
      ),
    ].map(([, what, seconds, milliseconds, left, deadline]) => ({
      what,
      armedMs: Number(seconds) * 1000 + Number(milliseconds),
      leftMs: Number(left),
      deadlineMs: Number(deadline),
    }));
    expect(armed.filter(({ what }) => what === "lock wait").length).toBeGreaterThanOrEqual(2);
    expect(armed.filter(({ what }) => what === "mint").length).toBeGreaterThanOrEqual(1);
    for (const timeout of armed) {
      expect(timeout.deadlineMs).toBe(bound);
      expect(timeout.armedMs).toBeLessThanOrEqual(timeout.leftMs);
      expect(timeout.leftMs).toBeLessThanOrEqual(bound);
    }
  });

  test("bounds every call but the preflight below the ten seconds Oh My Pi gives a `!command`", () => {
    const { keyCommand } = install();
    expect(callBoundMs(keyCommand)).toBeLessThan(ohMyPiCommandMs);
  });

  test("exempts the installer's preflight mint from the deadline, for hawk-token's first-run build", () => {
    // The fake's mint takes 11 s, past the 9500 ms every other call gives up at: the preflight
    // still kept the key that mint prints once its sleep is over, which `install` requires.
    install({ sleep: 11 });
  });

  test("keeps the installer's preflight out of the record, and says why it got no key once", () => {
    const run = join(dir, "preflight-refused");
    mkdirSync(join(run, "omp-home", ".omp"), { recursive: true });
    const result = Bun.spawnSync(
      [
        "bash",
        join(lib, "install-model-gateway.sh"),
        "--profile",
        "legion-e2e-test",
        "--home",
        join(run, "omp-home"),
        "--dest",
        join(run, "model-gateway"),
        "--cache-dir",
        join(run, "model-gateway-cache"),
      ],
      { env: env(join(run, "mints"), { mode: "refused" }) }
    );

    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(
      "the preflight got no gateway key: hawk-token key command: no key (failed): hawk-token exited 1 without a key: error: no usable hawk login"
    );
    // The preflight is no agent: a failed run's notes do not count it, nor repeat its reason.
    expect(existsSync(join(run, "model-gateway", "hawk-token.calls"))).toBe(false);
  });

  test("refuses any stage proof's evidence directory that is not empty, whether or not it reached its key command", () => {
    const evidence = join(dir, "evidence");
    expect(unserved("--fresh", evidence)).toMatchObject({ code: 0, stdout: "" });
    mkdirSync(evidence);
    expect(unserved("--fresh", evidence)).toMatchObject({ code: 0, stdout: "" });
    // An earlier Stage 4b run that stopped before its controller checkpoint left only these.
    writeFileSync(join(evidence, "transcript.log"), "== setup\n");
    const stopped = unserved("--fresh", evidence);
    expect(stopped.code).toBe(1);
    expect(stopped.stdout).toContain(`${evidence} is not an empty directory`);
    mkdirSync(join(evidence, "model-gateway"));
    expect(unserved("--fresh", evidence).code).toBe(1);
    // One it cannot list is refused too, never read as empty. At mode 0300 (write and search, no
    // read) a run it let through would append to the earlier run's transcript.log; at 000 it would
    // die at its first mkdir. Both need a non-root runner: root lists any directory.
    for (const mode of [0o300, 0o000]) {
      chmodSync(evidence, mode);
      try {
        const unlistable = unserved("--fresh", evidence);
        expect(unlistable.code).toBe(1);
        expect(unlistable.stdout).toContain(`${evidence} cannot be listed`);
        // ls's own error is the reason; the environment carries no LANG, so ls prints the C
        // locale's.
        expect(unlistable.stdout).toContain("Permission denied");
      } finally {
        chmodSync(evidence, 0o755);
      }
    }
  });

  test("a stage proof that refuses a reused evidence directory lists none of the earlier run's agents", async () => {
    // The earlier run's evidence directory: its key command, and a record whose last call starved.
    const { run: evidence, mints, keyCommand } = install();
    await call(keyCommand, join(evidence, "issue-workspace"), mints, {
      mode: "refused",
      role: "tester",
      generation: "1",
    });
    // Stage 3's teardown removes its containers; this run starts none, and needs no Docker.
    const stageBin = join(dir, "stage-bin");
    mkdirSync(stageBin, { recursive: true });
    writeFileSync(join(stageBin, "docker"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const stages: [string, Record<string, string>][] = [
      // Stage 3 checks its model route before anything else, so the run names one.
      [
        "stage3-devbox-workflow.sh",
        { STAGE3_EVIDENCE_DIR: evidence, LEGION_E2E_MODEL_ROUTE: "gateway" },
      ],
      [
        "stage3-4b13b-acceptance.sh",
        {
          ACCEPT_EVIDENCE_DIR: evidence,
          ACCEPT_PG_CONTAINER: "none",
          ACCEPT_PG_PORT: "1",
          ACCEPT_NATS_BIN: "/bin/false",
        },
      ],
    ];
    for (const [script, inputs] of stages) {
      const result = Bun.spawnSync(["bash", join(lib, "..", script)], {
        env: {
          ...bunCacheOff,
          PATH: `${stageBin}:${process.env.PATH}`,
          HOME: operatorHome,
          ...inputs,
        },
      });
      const stderr = result.stderr.toString();
      const scratch = /the run's scratch workspace(?:, kept for review,)? is (\S+)/.exec(
        stderr
      )?.[1];
      if (scratch !== undefined) rmSync(scratch, { recursive: true, force: true });
      expect(result.exitCode).toBe(1);
      expect(stderr).toContain(`FAIL setup: ${evidence} is not an empty directory`);
      // The earlier run's tester could not have failed this run's setup, which never started one.
      expect(`${result.stdout}${stderr}`).not.toContain("may have failed check");
    }
  });

  test("the profile's config.yml raises Oh My Pi's tool_call handler limit to 120 s, so AskGate's review fits inside it", () => {
    const { run } = install();
    const config = Bun.YAML.parse(
      readFileSync(join(run, "omp-home/.omp/profiles/legion-e2e-test/agent/config.yml"), "utf8")
    ) as { extensionHandlers?: { toolCallTimeoutMs?: unknown } };
    expect(config.extensionHandlers?.toolCallTimeoutMs).toBe(120_000);
  });
});
