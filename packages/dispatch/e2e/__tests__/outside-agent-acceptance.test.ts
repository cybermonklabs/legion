/** What the two legs of the outside agent's acceptance print when a row's target is absent:
 *  `e2e/acceptance/outside-agent-public.sh` (rows P1-P10, the leg a runner on the public internet
 *  runs) and `e2e/acceptance/outside-agent-tailnet.sh` (the pins, the scratch project's issue,
 *  rows T1-T5, the public leg's rows, the public leg's own last word, the sweep, the pins at the
 *  end and the gate, the leg a developer box runs). Each leg prints a TSV on stdout, a header
 *  `row want got verdict` and one line per row, and a row it could not run reads UNKNOWN with a
 *  `got` that names what it could not read. Every row UNKNOWN exits 2.
 *
 *  Each leg runs twice here. Once with no input at all, so every row names the input it lacks. Once
 *  with every input set and nothing behind any of them: each origin is a loopback port that
 *  nothing listens on, and PATH holds only the shell tools the legs read text with, no `omp`,
 *  `tmux`, `bun`, `gh`, `git`, `crane` or `agent-secrets`, so every row reaches its first read of
 *  a target and finds nothing.
 *
 *  The control is the same check run on a copy of each leg in which every `UNKNOWN` reads `PASS`
 *  and every `exit 2` reads `exit 0`: a leg that defaults to PASS. The check must fail on that
 *  copy, and the row says what it found, so a check that cannot fail fails here.
 *
 *  The blocks after those run the background shapes the legs start their children in, stopped by
 *  the shared `reap`, beside the shapes `reap` cannot stop and a pid that is no child of the leg,
 *  which `reap` must leave alone; the stand-in Dispatch's port and readiness under a squat, which
 *  takes over any port a listener of this process closed; and the tailnet leg against stand-ins of
 *  what it drives, reading the rows it judges by its own controls PASS where those controls answer
 *  and not PASS where they do not. The public leg's own judgments are
 *  `outside-agent-public.test.ts`'s.
 */

import { describe, expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";
import {
  acceptance,
  codexRows,
  copyLeg,
  type LegRun,
  publicLeg,
  publicRows,
  rowsOf,
  script,
  tailnetLeg,
  textToolPath,
  verdictsOf,
} from "./outside-agent-fixture";
import {
  fullLogin,
  inviteUrl,
  linkToken,
  liveStandInDispatches,
  type PreparedStandIn,
  passingControls,
  prepareStandIn,
  proxyRepository,
  type StandInRun,
  type StandIns,
  serviceGrant,
  unknownsCounted,
  unlistedPerson,
  workflowInputs,
  workflowRepository,
  writeIds,
} from "./outside-agent-tailnet-stand-ins";
import { closedPort } from "./outside-path-reach-fixtures";

const { c1, m1, c2, m2 } = writeIds;

/** The rows each leg prints, in order, after its header: the public leg's P1-P10, its
 *  revocations, its three T5 rows and its verdict; the tailnet leg's own rows, the public leg's
 *  P1-P10 as it read them, the public leg's own last word, the sweep, the pins at the end and the
 *  gate. */
const publicLegRows = [
  ...publicRows,
  "revocations",
  "T5-agent",
  "T5-cookie",
  "T5-passkey",
  "verdict",
];
const tailnetRows = [
  "pins",
  "issue",
  "T1",
  "T2",
  "T3",
  "T4",
  "T5",
  ...publicRows,
  "public",
  "sweep",
  "pins-end",
  "gate",
];

const withoutTools = skipWithout("bash", "curl");
// On tmpfs: the afterEach that removes a row's directories has Bun's 5 s for a hook, and on the
// shared disk one removal of a row's ~30 entries was measured at up to 17.5 s (median 2 ms, 30
// trials, load ~1500), against at most 16 ms on /dev/shm. The suite holds at most 233 entries at
// once (measured with the legs' own scratch under the same root), under 1 MiB at one 4 KiB page a
// file, so 8 MiB leaves room for 8 runs sharing the host.
const scratch = temporaryDirectories("outside-agent-acceptance", {
  root: memoryRoot({ need: 8 * 2 ** 20 }),
});

/** A copy of `leg` that defaults to PASS: every `UNKNOWN` reads `PASS` and every `exit 2` reads
 *  `exit 0`, in the leg and in what it sources. */
function passingCopy(leg: string): string {
  return copyLeg(scratch, leg, (text) =>
    text.replaceAll("UNKNOWN", "PASS").replaceAll("exit 2", "exit 0")
  );
}

async function runLeg(
  leg: string,
  args: readonly string[],
  env: Record<string, string>
): Promise<LegRun> {
  const { code, stdout, stderr } = await runBounded({
    argv: ["bash", leg, ...args],
    bound: "short",
    cwd: scratch("cwd"),
    env,
    from: import.meta.url,
  });
  return { code, stdout, stderr };
}

/** What is wrong with `run` as a leg's answer when every row's target is absent: each problem in
 *  words, none when every one of `rows` reads UNKNOWN in order under the header, names what it
 *  could not read, and the run exits 2. An empty stdout is a problem for every row, never a pass. */
function unknownProblems(run: LegRun, rows: readonly string[]): string[] {
  const problems: string[] = [];
  if (run.code !== 2) problems.push(`exit ${run.code}, want 2`);
  const lines = run.stdout.split("\n").filter((line) => line !== "");
  if (lines[0] !== "row\twant\tgot\tverdict") {
    problems.push(`the first line is ${JSON.stringify(lines[0] ?? "")}, want the header`);
  }
  const body = lines.slice(1);
  rows.forEach((row, index) => {
    const fields = (body[index] ?? "").split("\t");
    if (fields[0] !== row) {
      problems.push(`line ${index + 2} is ${JSON.stringify(body[index] ?? "")}, want row ${row}`);
      return;
    }
    if (fields.length !== 4) problems.push(`row ${row} has ${fields.length} fields, want 4`);
    if ((fields[1] ?? "").trim() === "") problems.push(`row ${row} names no want`);
    if ((fields[2] ?? "").trim() === "")
      problems.push(`row ${row} names nothing it could not read`);
    if (fields[3] !== "UNKNOWN") problems.push(`row ${row} reads ${fields[3]}, want UNKNOWN`);
  });
  if (body.length !== rows.length) {
    problems.push(`${body.length} rows, want ${rows.length}`);
  }
  return problems;
}

function noInput(): Record<string, string> {
  return {
    PATH: process.env.PATH ?? "",
    HOME: scratch("home"),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
  };
}

async function publicInputs(): Promise<Record<string, string>> {
  const origin = `http://127.0.0.1:${await closedPort()}`;
  const inputs = scratch("inputs");
  const targets = join(inputs, "targets.txt");
  writeFileSync(targets, `hub 127.0.0.1 ${await closedPort()}\n`);
  const approve = script(
    join(inputs, "approve"),
    '#!/bin/sh\necho "UNKNOWN: $1 no browser"\nexit 2\n'
  );
  return {
    PATH: textToolPath(scratch),
    HOME: scratch("home"),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
    OUTSIDE_ALLOW_HTTP: "1",
    OUTSIDE_DISPATCH_URL: origin,
    OUTSIDE_BROKER_URL: origin,
    OUTSIDE_LISTENER_URL: origin,
    OUTSIDE_PROJECT: "EXAMPLE",
    OUTSIDE_PERSON: "outside-e2e@example.com",
    OUTSIDE_PEER_SESSION: "example-peer",
    OUTSIDE_APPROVE_COMMAND: approve,
    OUTSIDE_PI_ENVOY_VERSION: "0.0.0",
    OUTSIDE_PATH_TARGETS: targets,
    OUTSIDE_REPOSITORY: "example-owner/example-repo",
    OUTSIDE_CANARY: "EXAMPLE_CANARY",
    OUTSIDE_AGENT_PUBLIC_TSV: join(inputs, "public.tsv"),
  };
}

async function tailnetInputs(): Promise<Record<string, string>> {
  const origin = `http://127.0.0.1:${await closedPort()}`;
  const inputs = scratch("inputs");
  const pins = join(inputs, "pins.txt");
  writeFileSync(
    pins,
    [
      `broker ${join(inputs, "absent-broker.yaml")} example:brokerImage`,
      `dispatch ${join(inputs, "absent-dispatch.yaml")} example:dispatchImage`,
      `edge-listener ${join(inputs, "absent-listener.yaml")} example:listenerImage`,
      "",
    ].join("\n")
  );
  return {
    PATH: textToolPath(scratch),
    HOME: scratch("home"),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
    OUTSIDE_ALLOW_HTTP: "1",
    OUTSIDE_PINS: pins,
    OUTSIDE_ENROLL_REFUSAL_COMMIT: "0123456789abcdef0123456789abcdef01234567",
    OUTSIDE_DISPATCH_COMMITS: "89abcdef0123456789abcdef0123456789abcdef",
    OUTSIDE_TAILNET_BROKER_URL: origin,
    OUTSIDE_TAILNET_DISPATCH_URL: origin,
    OUTSIDE_LISTENER_URL: origin,
    OUTSIDE_PERSON: "outside-e2e@example.com",
    OUTSIDE_CANARY: "EXAMPLE_CANARY",
    OUTSIDE_REPOSITORY: "example-owner/example-repo",
    OUTSIDE_WORKFLOW_REPO: "example-owner/example-deploy",
    OUTSIDE_WORKFLOW_INPUTS: "invite_url=https://dispatch.edge.example/enroll#example-token",
    OUTSIDE_TAILNET_TSV: join(inputs, "tailnet.tsv"),
  };
}

describe.skipIf(withoutTools)("the outside agent's acceptance legs", () => {
  test(
    "the public leg reads UNKNOWN for every row and exits 2 when it is given no input",
    async () => {
      const run = await runLeg(publicLeg, [], noInput());
      expect(unknownProblems(run, publicLegRows)).toEqual([]);
      for (const name of [
        "OUTSIDE_DISPATCH_URL",
        "OUTSIDE_PI_ENVOY_VERSION",
        "OUTSIDE_PATH_TARGETS",
      ]) {
        expect(run.stdout).toContain(name);
      }
    },
    // The leg, a bounded child under the short bound.
    rowLimit("short")
  );

  test(
    "the public leg reads UNKNOWN for every row and exits 2 when nothing answers behind its inputs",
    async () => {
      const env = await publicInputs();
      const run = await runLeg(publicLeg, [], env);
      expect(unknownProblems(run, publicLegRows)).toEqual([]);
      expect(run.stdout).toContain("omp");
      // The TSV it writes for the workflow's artifact is the one it prints.
      expect(readFileSync(env.OUTSIDE_AGENT_PUBLIC_TSV as string, "utf8")).toBe(run.stdout);
    },
    // The leg, a bounded child under the short bound; its P9 runs outside-path-reach.sh inside it.
    rowLimit("short")
  );

  test(
    "the tailnet leg reads UNKNOWN for every row and exits 2 when it is given no input",
    async () => {
      const run = await runLeg(tailnetLeg, [], noInput());
      expect(unknownProblems(run, tailnetRows)).toEqual([]);
      for (const name of [
        "the project key",
        "OUTSIDE_PINS",
        "OUTSIDE_ENROLL_REFUSAL_COMMIT",
        "OUTSIDE_DISPATCH_COMMITS",
        "OUTSIDE_TAILNET_DISPATCH_URL",
      ]) {
        expect(run.stdout).toContain(name);
      }
    },
    // The leg, a bounded child under the short bound.
    rowLimit("short")
  );

  test(
    "the tailnet leg reads UNKNOWN for every row and exits 2 when nothing answers behind its inputs",
    async () => {
      const env = await tailnetInputs();
      const run = await runLeg(tailnetLeg, ["EXAMPLE"], env);
      expect(unknownProblems(run, tailnetRows)).toEqual([]);
      expect(readFileSync(env.OUTSIDE_TAILNET_TSV as string, "utf8")).toBe(run.stdout);
    },
    // The leg, a bounded child under the short bound.
    rowLimit("short")
  );

  test(
    "the check fails on a copy of either leg that defaults to PASS",
    async () => {
      const runs = [
        await runLeg(passingCopy(publicLeg), [], noInput()),
        await runLeg(passingCopy(publicLeg), [], await publicInputs()),
        await runLeg(passingCopy(tailnetLeg), [], noInput()),
        await runLeg(passingCopy(tailnetLeg), ["EXAMPLE"], await tailnetInputs()),
      ];
      const rowsOfRun = [publicLegRows, publicLegRows, tailnetRows, tailnetRows];
      runs.forEach((run, index) => {
        const problems = unknownProblems(run, rowsOfRun[index] as string[]);
        expect(problems).toContain("exit 0, want 2");
        expect(problems.some((problem) => problem.endsWith("reads PASS, want UNKNOWN"))).toBe(true);
      });
    },
    // The four copies, one after another, each a bounded child under the short bound.
    rowLimit("short", "short", "short", "short")
  );
});

/* The shapes a leg starts its background children in, and the shared `reap` and `stop_child` that
 * stop them. Each shape's leaf is a `sleep` of a number no other process runs, so a leaf left
 * running after `reap` is found by that number. The shapes the legs use are a simple command, a
 * function whose last word is `exec`, and such a function behind a prefix assignment: `$!` names
 * the leaf itself, and `reap` stops it. The two shapes the legs no longer use are the control: a
 * subshell that does more after its command, and a function that runs its command without `exec`,
 * whose leaf `reap` cannot reach. The sixth and seventh are pids of `background` that are no child
 * of the shell, as a pid an ended child left becomes once another process holds it: `stop_child`
 * must leave the sixth running, and `reap` the seventh. */
describe.skipIf(skipWithout("bash", "sleep", "pgrep"))(
  "the shapes a leg starts its children in",
  () => {
    test(
      "reap stops every shape the legs use, not the two they no longer use, and never a pid that is no child",
      async () => {
        const base = `9${Math.floor(Math.random() * 1e6)
          .toString()
          .padStart(6, "0")}`;
        const sleep = hostTool("sleep") as string;
        const shapes = script(
          join(scratch("shapes"), "shapes.sh"),
          `#!/usr/bin/env bash
set -u
. '${join(acceptance, "outside-agent-lib.sh")}'
background=''
env -i '${sleep}' ${base}1 &
background="$background $!"
leaf() { exec '${sleep}' "$1"; }
leaf ${base}2 &
background="$background $!"
probe_as=x leaf ${base}3 &
background="$background $!"
( '${sleep}' ${base}4; echo done >/dev/null ) &
background="$background $!"
noexec() { '${sleep}' "$1"; }
noexec ${base}5 &
background="$background $!"
# Two processes that are no child of this shell: the parent of each, its command substitution, has
# ended. stop_child is asked to stop the first; reap finds the second on the list.
stranger=$( '${sleep}' ${base}6 >/dev/null 2>&1 & echo $! )
background="$background $stranger"
other=$( '${sleep}' ${base}7 >/dev/null 2>&1 & echo $! )
background="$background $other"
sleep 1
stop_child "$stranger"
reap
sleep 1
for n in 1 2 3 4 5 6 7; do
  pids=$(pgrep -f "^${sleep} ${base}$n\\$")
  if [ -n "$pids" ]; then
    echo "$n survived"
    # shellcheck disable=SC2086 # each pid a word
    kill $pids
  else
    echo "$n stopped"
  fi
done
`
        );
        const { code, stdout } = await runBounded({
          argv: ["bash", shapes],
          bound: "short",
          cwd: scratch("cwd"),
          env: { PATH: process.env.PATH ?? "" },
          from: import.meta.url,
        });
        expect({ code, stdout }).toEqual({
          code: 0,
          stdout: [
            "1 stopped",
            "2 stopped",
            "3 stopped",
            "4 survived",
            "5 survived",
            "6 survived",
            "7 survived",
            "",
          ].join("\n"),
        });
      },
      // The shape script, a bounded child under the short bound.
      rowLimit("short")
    );
  }
);

/** The prepared leg's one run; its stand-in Dispatch stops when it ends. */
async function runStandIn(prepared: PreparedStandIn): Promise<StandInRun> {
  try {
    const run = await runLeg(prepared.leg, ["EXAMPLE"], prepared.env);
    return {
      ...run,
      seen: prepared.dispatch.seen(),
      state: prepared.state,
      repo: prepared.repo,
    };
  } finally {
    prepared.dispatch.stop();
  }
}

/** Prepares each run, one at a time, then runs every prepared leg at once; every run answers, in
 *  the order given. A preparation that throws stops the stand-in Dispatch of each run already
 *  prepared before the throw goes on, so no stand-in outlives the row. */
async function runStandIns(...standIns: StandIns[]): Promise<StandInRun[]> {
  const prepared: PreparedStandIn[] = [];
  try {
    for (const each of standIns) prepared.push(await prepareStandIn(scratch, each));
  } catch (error) {
    for (const done of prepared) done.dispatch.stop();
    throw error;
  }
  return Promise.all(prepared.map(runStandIn));
}

/** The squatter's pid, the first line `squat`'s shell prints: a whole number above 0, or a throw
 *  that names what was printed. Never 0 or below, which `process.kill` reads as this process's
 *  whole group or as every process it may signal, so no kill is sent on a pid this cannot read. */
function squatterPid(printed: string): number {
  const first = printed.trim().split("\n")[0] ?? "";
  const pid = /^[0-9]+$/.test(first) ? Number(first) : 0;
  if (pid <= 0)
    throw new Error(`the squatter's pid could not be read from ${JSON.stringify(first)}`);
  return pid;
}

/** Starts a process that binds `port` and answers there as another run's stand-in Dispatch does
 *  (`/_ready` "ready", /healthz commit `another-run`), and waits until it answers there; returns
 *  the squatter's pid, or stops it and throws. The squatter tries the port for at most 10 s by the
 *  clock, and the wait reads it for at most 10 s by the clock, plus the 1 s of its last read, so a
 *  port that takes connections and never answers holds the wait for 11 s at most. It is
 *  synchronous because the listener stop it follows is: the port is taken before that stop
 *  returns. */
function squat(port: number): number {
  const squatter = `const port = Number(process.argv[1]); const until = Date.now() + 10000; while (Date.now() < until) { try { Bun.serve({ hostname: "127.0.0.1", port, fetch: (r) => new URL(r.url).pathname === "/_ready" ? new Response("ready") : Response.json({ ok: true, commit: "another-run", fleet_commit: "another-run" }) }); break; } catch { await Bun.sleep(25); } }`;
  const started = Bun.spawnSync([
    "bash",
    "-c",
    `nohup "$0" -e "$1" "$2" >/dev/null 2>&1 & echo $!; deadline=$((SECONDS + 10)); while [ "$SECONDS" -lt "$deadline" ]; do [ "$(curl -q -s --max-time 1 "http://127.0.0.1:$2/_ready")" = ready ] && exit 0; sleep 0.05; done; exit 1`,
    process.execPath,
    squatter,
    String(port),
  ]);
  const pid = squatterPid(started.stdout.toString());
  if (!started.success) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
    throw new Error(`the squatter did not take port ${port}`);
  }
  return pid;
}

/** Runs `body` with the next loopback listener this process stops taken over at once by another
 *  process (`squat`), one that answers as another run's stand-in Dispatch does: the race a port
 *  chosen by binding and closing a listener, and bound by a child later, is open to on a box where
 *  other runs start servers. Returns what `body` returned and the ports taken over; the squatters
 *  are stopped by their pids before it returns. */
async function withSquat<T>(body: () => Promise<T>): Promise<{ result: T; squatted: number[] }> {
  const listen = Bun.listen;
  const squatted: number[] = [];
  const squatters: number[] = [];
  let armed = true;
  Bun.listen = ((options: Parameters<typeof Bun.listen>[0]) => {
    const listener = listen(options);
    if (!armed || !("port" in listener)) return listener;
    armed = false;
    const port = Number(listener.port);
    const stop = listener.stop.bind(listener);
    listener.stop = (closeActiveConnections?: boolean) => {
      stop(closeActiveConnections);
      squatters.push(squat(port));
      squatted.push(port);
    };
    return listener;
  }) as typeof Bun.listen;
  try {
    return { result: await body(), squatted };
  } finally {
    Bun.listen = listen;
    for (const pid of squatters) {
      try {
        process.kill(pid, "SIGKILL");
      } catch {}
    }
  }
}

describe.skipIf(skipWithout("bash", "curl", "git", "bun"))(
  "the stand-in Dispatch's port and readiness",
  () => {
    test(
      "control: the squat takes a port a listener of this process closed, and answers there as another run's stand-in",
      async () => {
        const { result, squatted } = await withSquat(async () => {
          const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
          const { port } = listener;
          listener.stop(true);
          return { port, ready: await (await fetch(`http://127.0.0.1:${port}/_ready`)).text() };
        });
        expect({ squatted, ready: result.ready }).toEqual({
          squatted: [result.port],
          ready: "ready",
        });
      },
      {
        // It drives one squat, a synchronous wait its own clock ends within 11 s, which Bun's 5 s
        // default of a plain `bun test` would cut short; 20 s leaves that wait room under load.
        timeout: 20_000,
      }
    );

    test("squat throws on a squatter pid it cannot read, before any kill: an empty line, 0 and -1 are refused, and a whole number above 0 is read", () => {
      // squat's shell stands in printing each line as its pid and failing, which is the path that
      // kills the squatter; process.kill records every signal it is asked to send and sends none.
      const spawnSync = Bun.spawnSync;
      const kill = process.kill;
      const sent: unknown[] = [];
      const read: unknown[] = [];
      process.kill = ((pid: number, signal?: string | number) => {
        sent.push([pid, signal]);
        return true;
      }) as typeof process.kill;
      try {
        for (const printed of ["", "\n", "0\n", "-1\n"]) {
          Bun.spawnSync = (() => ({
            stdout: Buffer.from(printed),
            success: false,
          })) as unknown as typeof Bun.spawnSync;
          try {
            read.push(squat(1));
          } catch (error) {
            read.push(String(error));
          }
        }
        read.push(squatterPid("4242\n"));
      } finally {
        Bun.spawnSync = spawnSync;
        process.kill = kill;
      }
      expect({ read, sent }).toEqual({
        read: [
          `Error: the squatter's pid could not be read from ""`,
          `Error: the squatter's pid could not be read from ""`,
          `Error: the squatter's pid could not be read from "0"`,
          `Error: the squatter's pid could not be read from "-1"`,
          4242,
        ],
        sent: [],
      });
    });

    test(
      "the squat's wait is held by the clock: on a port that takes connections and never answers, it gives up within 15 s",
      () => {
        // The kernel takes each connection into this listener's queue while the wait blocks this
        // file's event loop, so every read connects and then hears nothing until its 1 s passes.
        const holder = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
        const began = performance.now();
        let thrown = "";
        try {
          squat(holder.port);
        } catch (error) {
          thrown = String(error);
        } finally {
          holder.stop(true);
        }
        const seconds = (performance.now() - began) / 1000;
        expect({ thrown, withinTheCap: seconds < 15, seconds }).toEqual({
          thrown: `Error: the squatter did not take port ${holder.port}`,
          withinTheCap: true,
          seconds: expect.any(Number),
        });
      },
      {
        // It drives one squat against a port that never answers, which the squat's own clock ends
        // within 11 s by design; 20 s is past the 15 s it asserts, so the assertion fails first.
        timeout: 20_000,
      }
    );

    test(
      "a prepared run's origin serves its own stand-in Dispatch under the squat, and the stand-in is given no port: no port of it is one this process chose and closed",
      async () => {
        const { result, squatted } = await withSquat(async () => {
          let prepared: PreparedStandIn;
          try {
            prepared = await prepareStandIn(scratch, passingControls);
          } catch (error) {
            return { thrown: String(error) };
          }
          try {
            const answer = (await (await fetch(`${prepared.dispatch.origin}/healthz`)).json()) as {
              commit?: string;
            };
            // The settings the stand-in runs with, as the kernel holds them: a port handed to it
            // by any means the squat of `Bun.listen` cannot see (`Bun.serve`, `node:net`) would
            // show here as a `port` of its settings.
            const settings = readFileSync(`/proc/${prepared.dispatch.pid}/environ`, "utf8")
              .split("\0")
              .filter((entry) => entry.startsWith("OUTSIDE_STAND_IN_DISPATCH="))
              .map(
                (entry) =>
                  JSON.parse(entry.slice("OUTSIDE_STAND_IN_DISPATCH=".length)) as Record<
                    string,
                    unknown
                  >
              );
            return {
              built: prepared.repo.built,
              served: answer.commit,
              handed: settings.map((each) => each.port ?? null),
            };
          } finally {
            prepared.dispatch.stop();
          }
        });
        expect({ ...result, squatted }).toEqual({
          built: expect.any(String),
          served: "built" in result ? result.built : "the run's own build",
          handed: [null],
          squatted: [],
        });
      },
      {
        // It drives a preparation (a git checkout, and the stand-in's start, whose readiness its own
        // clock ends within 11 s) and at most one squat, also 11 s by its clock; 30 s holds both.
        timeout: 30_000,
      }
    );
  }
);

describe.skipIf(skipWithout("bash", "curl", "git", "base64", "bun"))(
  "the tailnet leg's own controls, against stand-ins",
  () => {
    test(
      "T3, P7 and the sweep pass only with the full member's browser answered, and T2 only with the member kind's refusal",
      async () => {
        const [answered, refused] = await runStandIns(passingControls, {
          controls: "refused",
          canaryRefusal: "403 REQUESTER_NOT_LISTED: requester not listed",
          dispatchCommits: "held",
        });
        if (answered === undefined || refused === undefined) throw new Error("two runs, want two");
        // The verdicts beside the whole TSV, so a mismatch prints every row the leg wrote.
        expect({ verdicts: verdictsOf(answered.stdout), stdout: answered.stdout }).toMatchObject({
          verdicts: {
            pins: "PASS",
            T1: "PASS",
            T2: "PASS",
            T3: "PASS",
            T4: "PASS",
            P7: "PASS",
            public: "PASS",
            sweep: "PASS",
            "pins-end": "PASS",
          },
        });
        const pass = rowsOf(answered.stdout);
        expect(pass.get("P7")?.got).toContain(
          "the positive control: the full member's browser minted an agent token, 201 (revoked: 204), and approved a machine login, 200 (revoked: 200)"
        );
        expect(pass.get("sweep")?.got).toContain(
          `the full member's browser session read ${proxyRepository}'s workflow runs through the proxy: 200`
        );
        // The writer's line in its `write <began> <ended> <comment id> <message id> delivered` form.
        expect(pass.get("sweep")?.got).toContain(
          `the writer's write from 2026-01-01T00:00:04Z to 2026-01-01T00:00:05Z (comment ${c1}, message ${m1}) was delivered`
        );
        // T3 enrolled through the full member's helper, whose broker is the one T2 asked.
        expect(readFileSync(join(answered.state, "t3-operator"), "utf8")).toBe(`${fullLogin}\n`);
        // Each thing the control made is revoked again, the sweep's fills read the cookie, and the
        // leg stopped the sweep's writer itself.
        expect(answered.seen).toContain("DELETE /api/v1/me/agent-tokens/token-1");
        expect(answered.seen).toContain("POST /api/v1/machine-logins/cred-full-0001/revoke");
        expect(existsSync(join(answered.state, "fills-read-the-cookie"))).toBe(true);
        expect(existsSync(join(answered.state, "writer-stopped"))).toBe(true);
        // P4, P5, P6 and T5 have no peer, so the gate is held: P6's PASS without the session
        // outside the project to stand behind it reads UNKNOWN.
        expect(pass.get("P6")).toMatchObject({ verdict: "UNKNOWN" });
        expect(answered.code).toBe(2);

        // The control of those PASSes: the same run with the controls refused, and the canary's
        // session refused for a reason other than the member kind.
        expect({ verdicts: verdictsOf(refused.stdout), stdout: refused.stdout }).toMatchObject({
          verdicts: { T1: "PASS", T2: "FAIL", T3: "UNKNOWN", P7: "UNKNOWN", sweep: "UNKNOWN" },
        });
        const held = rowsOf(refused.stdout);
        expect(held.get("T2")?.got).toContain("the canary got no value for another reason");
        expect(held.get("T3")?.got).toContain("no full member's helper at the broker");
        expect(held.get("P7")?.got).toContain("no positive control");
        expect(held.get("sweep")?.got).toContain("its GitHub proxy rows have no positive control");
        expect(refused.code).toBe(1);
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "the pins row fails, and no leg starts, when Dispatch's build lacks a commit the check needs, and reads UNKNOWN for a commit list of blanks or of a word that is no commit id",
      async () => {
        const [run, blank, malformed] = await runStandIns(
          { ...passingControls, dispatchCommits: "lacking" },
          { ...passingControls, dispatchCommits: "blank" },
          { ...passingControls, dispatchCommits: "malformed" }
        );
        if (run === undefined || blank === undefined || malformed === undefined) {
          throw new Error("three runs, want three");
        }
        const rows = rowsOf(run.stdout);
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: { pins: "FAIL" },
        });
        expect(rows.get("pins")?.got).toContain(
          `Dispatch's build ${run.repo.built} does not hold ${run.repo.apart}`
        );
        expect(rows.get("T1")?.got).toContain("not run: a pin lacks a change the check needs");
        // Dispatch is asked only the build it serves.
        expect(run.seen).toEqual(["GET /healthz"]);
        expect(run.code).toBe(1);
        // A list that holds Dispatch's build to nothing, or to a word git reads as anything,
        // starts no leg.
        for (const [each, said] of [
          [blank, "OUTSIDE_DISPATCH_COMMITS names no commit"],
          [malformed, "HEAD is no 40-character commit id"],
        ] as const) {
          expect({ verdicts: verdictsOf(each.stdout), stdout: each.stdout }).toMatchObject({
            verdicts: { pins: "UNKNOWN", T1: "UNKNOWN" },
          });
          expect(rowsOf(each.stdout).get("pins")?.got).toContain(said);
          expect(each.seen).toEqual([]);
        }
      },
      // The three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "the pins hold the gate when Dispatch serves another build than its pin, is half rolled out, a pin moves between the legs, the pins are local files, or the pins file names one deployment twice",
      async () => {
        const [other, split, moves, local, twice] = await runStandIns(
          { ...passingControls, serving: "other" },
          { ...passingControls, serving: "split" },
          { ...passingControls, pins: "moves" },
          { ...passingControls, pins: "local" },
          { ...passingControls, pins: "duplicate" }
        );
        if (
          other === undefined ||
          split === undefined ||
          moves === undefined ||
          local === undefined ||
          twice === undefined
        ) {
          throw new Error("five runs, want five");
        }
        // Two `dispatch` lines: only one image could be held, so the pins are not read; the
        // control is the answered run of the first test of this suite, one line a name.
        expect({ verdicts: verdictsOf(twice.stdout), stdout: twice.stdout }).toMatchObject({
          verdicts: { pins: "UNKNOWN", T1: "UNKNOWN" },
        });
        expect(rowsOf(twice.stdout).get("pins")?.got).toContain("has more than one dispatch line");
        expect({ verdicts: verdictsOf(other.stdout), stdout: other.stdout }).toMatchObject({
          verdicts: { pins: "UNKNOWN", T1: "UNKNOWN" },
        });
        expect(rowsOf(other.stdout).get("pins")?.got).toContain(
          `Dispatch serves ${other.repo.apart}, and its pin names ${other.repo.built}`
        );
        expect({ verdicts: verdictsOf(split.stdout), stdout: split.stdout }).toMatchObject({
          verdicts: { pins: "UNKNOWN", T1: "UNKNOWN" },
        });
        expect(rowsOf(split.stdout).get("pins")?.got).toContain(
          `not every Dispatch process serves ${split.repo.built}`
        );
        // A pin that moved at the deployment repository's main during the run holds the gate
        // and nothing else; the control is the answered run of the first test of this suite, whose
        // pins-end passes.
        expect({ verdicts: verdictsOf(moves.stdout), stdout: moves.stdout }).toMatchObject({
          verdicts: { pins: "PASS", T1: "PASS", "pins-end": "UNKNOWN" },
        });
        expect(rowsOf(moves.stdout).get("pins-end")?.got).toContain("a pin moved between the legs");
        expect({ verdicts: verdictsOf(local.stdout), stdout: local.stdout }).toMatchObject({
          verdicts: { pins: "PASS", "pins-end": "UNKNOWN" },
        });
        expect(rowsOf(local.stdout).get("pins-end")?.got).toContain("OUTSIDE_PINS_REPO is unset");
      },
      // The five runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short", "short"] })
    );

    test(
      "without the full member's cookie the sweep's proxy reads use their token: PASS where Dispatch takes it, UNKNOWN naming the refusal where a build reads the proxy as a browser's person alone, and UNKNOWN with fills that lack a proxy read's line",
      async () => {
        const [noCookie, refused, gap] = await runStandIns(
          { ...passingControls, cookie: "absent" },
          { ...passingControls, cookie: "absent", proxyRefusesToken: true },
          {
            ...passingControls,
            fillsLeaveOut: "GET /api/v1/github/app-installation/{owner}/{repo}",
          }
        );
        if (noCookie === undefined || refused === undefined || gap === undefined) {
          throw new Error("three runs, want three");
        }
        // Without the cookie the sweep runs on the full member's token, its fills' proxy reads
        // and the box's proxy control among them, and passes where Dispatch takes the token; P7
        // has no positive control, which only a browser session makes.
        expect({ verdicts: verdictsOf(noCookie.stdout), stdout: noCookie.stdout }).toMatchObject({
          verdicts: { sweep: "PASS", P7: "UNKNOWN" },
        });
        expect(rowsOf(noCookie.stdout).get("sweep")?.got).toContain(
          "the full member's token read example-owner/private-repo's workflow runs through the proxy: 200"
        );
        expect(existsSync(join(noCookie.state, "fills-read-the-cookie"))).toBe(false);
        expect(existsSync(join(noCookie.state, "fills-read-the-token"))).toBe(true);
        expect(existsSync(join(noCookie.state, "writer-stopped"))).toBe(true);
        // A build whose proxy reads a browser's person alone refuses the token: the fills hold no
        // proxy read's line, and the row reads UNKNOWN naming that refusal.
        expect({ verdicts: verdictsOf(refused.stdout), stdout: refused.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN" },
        });
        expect(rowsOf(refused.stdout).get("sweep")?.got).toContain(
          "was refused 403 OAUTH_SCOPE_REQUIRED: this build of Dispatch reads the proxy as a browser's person alone"
        );
        expect(existsSync(join(refused.state, "fills-without-cookie"))).toBe(true);
        // Fills that lack a proxy read's full-member answer hold the sweep, and only the sweep.
        expect({ verdicts: verdictsOf(gap.stdout), stdout: gap.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN", P7: "PASS" },
        });
        expect(rowsOf(gap.stdout).get("sweep")?.got).toContain(
          "the sweep's fills hold no full member's answer that can fail for GET /api/v1/github/app-installation/{owner}/{repo}"
        );
      },
      // The three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "the sweep counts no write the writer did not deliver nor a line of the old form, and reads UNKNOWN for a fills file fills mode refused and for a runner's UNKNOWN",
      async () => {
        const [undelivered, oldForm, refusedFills, unlisted] = await runStandIns(
          {
            ...passingControls,
            sweep: {
              writer: `write 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z ${c1} ${m1} not delivered`,
            },
          },
          // The form before #4608 timed the write's two ends: one time, inside the window.
          {
            ...passingControls,
            sweep: { writer: `write 2026-01-01T00:00:05Z ${c1} ${m1} delivered` },
          },
          { ...passingControls, sweep: { fillsRefused: true } },
          {
            ...passingControls,
            sweep: {
              verdict:
                "UNKNOWN: GET /api/v1/example is a route the deployment's own index lists and routes.txt lacks, which the sweep never called",
              exit: 2,
            },
          }
        );
        if (
          undelivered === undefined ||
          oldForm === undefined ||
          refusedFills === undefined ||
          unlisted === undefined
        ) {
          throw new Error("four runs, want four");
        }
        // A write inside the window the full member's feed was not sent is a dropped write; a line
        // of the old form has a second stamp this leg cannot read. Neither is a control.
        for (const [run, said] of [
          [undelivered, "was not delivered, so the member's feed lost one"],
          [oldForm, "the writer printed a line this leg cannot read"],
        ] as const) {
          expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { sweep: "UNKNOWN" },
          });
          expect(rowsOf(run.stdout).get("sweep")?.got).toContain(said);
        }
        // Fills mode refuses to write a file that lacks a fill, so no fills reach the runner.
        expect({
          verdicts: verdictsOf(refusedFills.stdout),
          stdout: refusedFills.stdout,
        }).toMatchObject({ verdicts: { sweep: "UNKNOWN" } });
        expect(rowsOf(refusedFills.stdout).get("sweep")?.got).toContain(
          "the sweep's fills mode made no file: UNKNOWN: fills mode writes no file that lacks a fill"
        );
        // A runner's UNKNOWN, a route the deployment lists and routes.txt lacks among them, stays
        // UNKNOWN.
        expect({ verdicts: verdictsOf(unlisted.stdout), stdout: unlisted.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN" },
        });
        expect(rowsOf(unlisted.stdout).get("sweep")?.got).toContain(
          "the sweep exited 2: UNKNOWN: GET /api/v1/example is a route the deployment's own index lists and routes.txt lacks"
        );
      },
      // The four runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short"] })
    );

    test(
      "the sweep counts a delivered write only when it began after the live window opened and ended before it closed, and holds a write inside it that was not written",
      async () => {
        // The runner's window is 2026-01-01T00:00:00Z to 2026-01-01T00:00:09Z; the answered run of
        // the first test of this suite, a write from :04 to :05, is the control.
        const lines = [
          [
            `write 2025-12-31T23:59:59Z 2026-01-01T00:00:01Z ${c1} ${m1} delivered`,
            "no write of the writer was delivered inside its live window",
          ],
          [
            `write 2026-01-01T00:00:08Z 2026-01-01T00:00:10Z ${c1} ${m1} delivered`,
            "no write of the writer was delivered inside its live window",
          ],
          [
            "write 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z - not written",
            "was not delivered, so the member's feed lost one",
          ],
        ] as const;
        const runs = await runStandIns(
          ...lines.map(([writer]) => ({ ...passingControls, sweep: { writer } }))
        );
        lines.forEach(([writer, said], index) => {
          const run = runs[index];
          if (run === undefined) throw new Error(`run ${index} is missing`);
          expect({ writer, verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { sweep: "UNKNOWN" },
          });
          expect(rowsOf(run.stdout).get("sweep")?.got).toContain(said);
        });
      },
      // The three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "a writer line whose stamps are not of date's form, that began after it ended, or whose ids are not of Dispatch's form, holds the sweep beside a delivered write",
      async () => {
        // Each beside a delivered write from :04 to :05 inside the window, which alone passes (the
        // answered run of the first test of this suite): a line this leg cannot read may hide a
        // dropped write.
        const good = `write 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z ${c1} ${m1} delivered`;
        const lines = [
          `write 9999 0000 ${c2} ${m2} delivered`,
          // Stamps of another form that compare as text inside the window and in order: only
          // their form tells them apart.
          `write 2026-01-01T00:00:04z 2026-01-01T00:00:05z ${c2} ${m2} delivered`,
          `write 2026-01-01T00:00:10Z 2026-01-01T00:00:05Z ${c2} ${m2} delivered`,
          `write 2026-01-01T00:00:06Z 2026-01-01T00:00:05Z ${c2} ${m2} delivered`,
          // Ids that are not of the form Dispatch gives them, as #4608's join refuses them.
          "write 2026-01-01T00:00:06Z 2026-01-01T00:00:07Z cmt-1 msg-1 delivered",
        ];
        const runs = await runStandIns(
          ...lines.map((bad) => ({ ...passingControls, sweep: { writer: `${good}\n${bad}` } }))
        );
        lines.forEach((bad, index) => {
          const run = runs[index];
          if (run === undefined) throw new Error(`run ${index} is missing`);
          expect({ bad, verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { sweep: "UNKNOWN" },
          });
          expect(rowsOf(run.stdout).get("sweep")?.got).toContain(
            `the writer printed a line this leg cannot read`
          );
          expect(rowsOf(run.stdout).get("sweep")?.got).toContain(bad);
        });
      },
      // The five runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short", "short"] })
    );

    test(
      "one delivered write inside the window is no control beside a dropped one, nor when its own line also says not delivered",
      async () => {
        const [beside, duplicated] = await runStandIns(
          {
            ...passingControls,
            sweep: {
              writer: `write 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z ${c1} ${m1} delivered\nwrite 2026-01-01T00:00:06Z 2026-01-01T00:00:07Z ${c2} ${m2} not delivered`,
            },
          },
          {
            ...passingControls,
            sweep: {
              writer: `write 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z ${c1} ${m1} not delivered\nwrite 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z ${c1} ${m1} delivered`,
            },
          }
        );
        if (beside === undefined || duplicated === undefined) throw new Error("two runs, want two");
        // The control is the answered run of the first test of this suite: one delivered write
        // and no other.
        for (const run of [beside, duplicated]) {
          expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { sweep: "UNKNOWN" },
          });
          expect(rowsOf(run.stdout).get("sweep")?.got).toContain(
            "was not delivered, so the member's feed lost one"
          );
        }
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "a dropped write outside the window, across its opening, across its closing or wholly before it, holds the sweep beside a delivered write inside it",
      async () => {
        // The window is :00 to :09; each dropped line beside a delivered write from :04 to :05,
        // which alone passes (the answered run of the first test of this suite).
        const good = `write 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z ${c1} ${m1} delivered`;
        const dropped = [
          `write 2025-12-31T23:59:58Z 2026-01-01T00:00:02Z ${c2} ${m2} not delivered`,
          `write 2026-01-01T00:00:07Z 2026-01-01T00:00:11Z ${c2} ${m2} not delivered`,
          "write 2025-12-31T23:59:50Z 2025-12-31T23:59:51Z - not written",
        ];
        const runs = await runStandIns(
          ...dropped.map((line) => ({ ...passingControls, sweep: { writer: `${good}\n${line}` } }))
        );
        // Each line's sweep row at once, so a failure names every line that read otherwise.
        const read = dropped.map((line, index) => {
          const run = runs[index];
          if (run === undefined) throw new Error(`run ${index} is missing`);
          return { line, sweep: rowsOf(run.stdout).get("sweep") };
        });
        expect(read).toMatchObject(
          dropped.map((line) => ({
            line,
            sweep: {
              verdict: "UNKNOWN",
              got: expect.stringContaining("was not delivered, so the member's feed lost one"),
            },
          }))
        );
      },
      // The three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "the sweep reads UNKNOWN for each refusal a runner makes before its first request, and the leg sends no fills of another version than 4",
      async () => {
        // The runner's own refusals before its first request, as #4608 words them.
        const refusals = [
          `UNKNOWN: SWEEP_FILLS_FILE was made from another route index: its routes.txt's SHA-256 is ${"a".repeat(64)}, and this run's is ${"c".repeat(64)}`,
          "UNKNOWN: SWEEP_FILLS_FILE was made at another deployment: the member's own projects were created at other times there (fills bbbbbbbbbbbb, here dddddddddddd)",
          "UNKNOWN: SWEEP_LIVE_SECONDS must be at least 45 on a runner, whose live window leaves the writer's 15 s delivery bound before its feed closes and holds two of its writes at 15 s: 20",
        ];
        const runs = await runStandIns(
          ...refusals.map((verdict) => ({ ...passingControls, sweep: { verdict, exit: 2 } })),
          { ...passingControls, sweep: { fillsVersion: 3 } }
        );
        refusals.forEach((verdict, index) => {
          const run = runs[index];
          if (run === undefined) throw new Error(`run ${index} is missing`);
          expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { sweep: "UNKNOWN" },
          });
          expect(rowsOf(run.stdout).get("sweep")?.got).toContain(`the sweep exited 2: ${verdict}`);
        });
        // Fills of another version stay on the box: no run gets them, and the sweep row says why.
        const otherVersion = runs[3];
        if (otherVersion === undefined) throw new Error("run 3 is missing");
        expect({
          verdicts: verdictsOf(otherVersion.stdout),
          stdout: otherVersion.stdout,
        }).toMatchObject({ verdicts: { sweep: "UNKNOWN" } });
        expect(rowsOf(otherVersion.stdout).get("sweep")?.got).toContain(
          "the sweep beside this leg wrote fills of another version than 4"
        );
      },
      // The four runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short"] })
    );

    test(
      "T1 reads a login nobody approved UNKNOWN, and T4 counts a revoke row with no verdict as UNKNOWN and reads a retried revoke's FAIL then PASS as FAIL",
      async () => {
        const [control, expired, noVerdict, retried] = await runStandIns(
          passingControls,
          { ...passingControls, approve: false },
          { ...passingControls, revokeVerdicts: ["SKIPPED"] },
          { ...passingControls, revokeVerdicts: ["FAIL", "PASS"] }
        );
        if (
          control === undefined ||
          expired === undefined ||
          noVerdict === undefined ||
          retried === undefined
        ) {
          throw new Error("four runs, want four");
        }
        expect(rowsOf(control.stdout).get("T4")).toMatchObject({ verdict: "PASS" });
        expect({ verdicts: verdictsOf(expired.stdout), stdout: expired.stdout }).toMatchObject({
          verdicts: { T1: "UNKNOWN" },
        });
        expect(rowsOf(expired.stdout).get("T1")?.got).toContain(
          "was never approved: the login expired"
        );
        expect({ verdicts: verdictsOf(noVerdict.stdout), stdout: noVerdict.stdout }).toMatchObject({
          verdicts: { T4: "UNKNOWN" },
        });
        expect(rowsOf(noVerdict.stdout).get("T4")?.got).toContain(
          "reads SKIPPED, which is no verdict"
        );
        // The same run but for that row: the gate counts it, so it can never exit 0 on it.
        expect(unknownsCounted(noVerdict.stdout)).toBe(unknownsCounted(control.stdout) + 1);
        // A revoke that failed and was retried: its worst copy is read, and the later PASS hides
        // nothing; the control is the first run, one PASS.
        expect({ verdicts: verdictsOf(retried.stdout), stdout: retried.stdout }).toMatchObject({
          verdicts: { T4: "FAIL" },
        });
        expect(rowsOf(retried.stdout).get("T4")?.got).toContain("reads FAIL");
      },
      // The four runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short"] })
    );

    test(
      "T5 fails on a trade answered past its bound after a removal the list confirms, and reads UNKNOWN on one within it",
      async () => {
        const [late, inTime] = await runStandIns(
          { ...passingControls, window: { lastAnsweredAfter: 1000 } },
          { ...passingControls, window: { lastAnsweredAfter: 0 } }
        );
        if (late === undefined || inTime === undefined) throw new Error("two runs, want two");
        expect({ verdicts: verdictsOf(late.stdout), stdout: late.stdout }).toMatchObject({
          verdicts: { T5: "FAIL" },
        });
        expect(rowsOf(late.stdout).get("T5")?.got).toContain("a trade was answered");
        expect(late.seen).toContain("POST /_remove");
        // The control: the last answered trade within the bound says nothing either way.
        expect({ verdicts: verdictsOf(inTime.stdout), stdout: inTime.stdout }).toMatchObject({
          verdicts: { T5: "UNKNOWN" },
        });
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "the public row holds the gate to the public leg's own verdict and end, a later PASS of a row hides no earlier FAIL, and P6's sub crossing stands",
      async () => {
        const [failed, silent, failedExit0, duplicated] = await runStandIns(
          {
            ...passingControls,
            publicVerdict: "FAIL",
            publicExit: 1,
            p6: "FAIL a relay sub to example-peer's subject was granted; peer_listed=yes foreign_listed=no foreign_send=refused sub=granted",
          },
          { ...passingControls, publicVerdict: "absent" },
          { ...passingControls, publicVerdict: "FAIL", publicExit: 0 },
          { ...passingControls, duplicate: { name: "P5", first: "FAIL", second: "PASS" } }
        );
        if (
          failed === undefined ||
          silent === undefined ||
          failedExit0 === undefined ||
          duplicated === undefined
        ) {
          throw new Error("four runs, want four");
        }
        expect({ verdicts: verdictsOf(failed.stdout), stdout: failed.stdout }).toMatchObject({
          verdicts: { public: "FAIL", P6: "FAIL" },
        });
        expect(rowsOf(failed.stdout).get("public")?.got).toContain("verdict row FAIL");
        expect(failed.code).toBe(1);
        expect({ verdicts: verdictsOf(silent.stdout), stdout: silent.stdout }).toMatchObject({
          verdicts: { public: "UNKNOWN" },
        });
        expect(rowsOf(silent.stdout).get("public")?.got).toContain("verdict row absent");
        // The verdict row's FAIL holds the public row by itself, whatever the leg's exit.
        expect({
          verdicts: verdictsOf(failedExit0.stdout),
          stdout: failedExit0.stdout,
        }).toMatchObject({ verdicts: { public: "FAIL" } });
        expect(rowsOf(failedExit0.stdout).get("public")?.got).toContain("verdict row FAIL");
        // A row the public TSV holds twice reads as the worse of the two.
        expect({
          verdicts: verdictsOf(duplicated.stdout),
          stdout: duplicated.stdout,
        }).toMatchObject({ verdicts: { P5: "FAIL" } });
        expect(rowsOf(duplicated.stdout).get("P5")?.got).toContain("what P5 read first");
      },
      // The four runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short"] })
    );

    test(
      "the gate counts only PASS: a Codex row the public TSV holds as UNKNOWN holds it, and the gate names that row; C4's PASS is held as P6's is, without the session outside the project",
      async () => {
        const [control, unknown] = await runStandIns(passingControls, {
          ...passingControls,
          verdicts: { C2: "UNKNOWN" },
        });
        if (control === undefined || unknown === undefined) throw new Error("two runs, want two");
        // The control: every C row PASS in the public TSV, so the gate names none of them but
        // C4. No peer starts here, so no session outside the project stands behind C4's control
        // send, and its PASS reads UNKNOWN, as P6's does.
        expect({ verdicts: verdictsOf(control.stdout), stdout: control.stdout }).toMatchObject({
          verdicts: Object.fromEntries(
            codexRows.map((name) => [name, name === "C4" ? "UNKNOWN" : "PASS"])
          ),
        });
        expect(rowsOf(control.stdout).get("C4")?.got).toContain(
          "PASS as the public leg read it, unproven here: its control's session outside the project: "
        );
        expect(rowsOf(control.stdout).get("gate")?.got).not.toContain("C2");
        // C2 UNKNOWN: copied as it is, counted, named by the gate, and the leg exits non-zero.
        expect({ verdicts: verdictsOf(unknown.stdout), stdout: unknown.stdout }).toMatchObject({
          verdicts: { C2: "UNKNOWN", gate: "UNKNOWN" },
        });
        expect(unknownsCounted(unknown.stdout)).toBe(unknownsCounted(control.stdout) + 1);
        expect(rowsOf(unknown.stdout).get("gate")?.got).toContain("C2 UNKNOWN");
        expect(unknown.code).not.toBe(0);
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "the workflow gets only the inputs its file declares, on stdin, the sweep's project among them, only in a private or internal repository, and a refused dispatch holds the gate at once",
      async () => {
        const [started, refused, inPublic, unread, internal, failedView] = await runStandIns(
          { ...passingControls, workflow: {} },
          { ...passingControls, workflow: { inputs: "edge_host=dispatch.edge.example" } },
          { ...passingControls, workflow: { visibility: "PUBLIC" } },
          { ...passingControls, workflow: { visibility: "" } },
          { ...passingControls, workflow: { visibility: "INTERNAL" } },
          { ...passingControls, workflow: { visibility: "INTERNAL", viewExit: 1 } }
        );
        if (
          started === undefined ||
          refused === undefined ||
          inPublic === undefined ||
          unread === undefined ||
          internal === undefined ||
          failedView === undefined
        ) {
          throw new Error("six runs, want six");
        }
        // An INTERNAL repository, which only the owning enterprise's members read, gets its run.
        expect({ verdicts: verdictsOf(internal.stdout), stdout: internal.stdout }).toMatchObject({
          verdicts: { T1: "PASS", public: "PASS" },
        });
        expect(existsSync(join(internal.state, "workflow-args"))).toBe(true);
        // A repository read as neither PRIVATE nor INTERNAL gets no run, nor one whose `gh repo
        // view` printed INTERNAL and exited 1: that view read nothing. The controls are the first
        // run, in a PRIVATE one, and the one above.
        for (const [run, said] of [
          [inPublic, "is PUBLIC, not PRIVATE or INTERNAL"],
          [unread, "could not be read"],
          [failedView, "gh repo view exited 1"],
        ] as const) {
          expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { T1: "UNKNOWN", public: "UNKNOWN" },
          });
          expect(rowsOf(run.stdout).get("public")?.got).toContain(said);
          expect(existsSync(join(run.state, "workflow-args"))).toBe(false);
        }
        // The words name no input: those reach `gh` on its stdin alone.
        expect(
          readFileSync(join(started.state, "workflow-args"), "utf8").trim().split("\n")
        ).toEqual(["workflow", "run", "outside-edge-e2e.yaml", "-R", workflowRepository, "--json"]);
        const inputs = JSON.parse(
          readFileSync(join(started.state, "workflow-inputs.json"), "utf8")
        ) as Record<string, string>;
        expect(Object.keys(inputs).filter((name) => !workflowInputs.includes(name))).toEqual([]);
        expect(inputs).toMatchObject({
          checks: "all",
          project: "EXAMPLE",
          tailnet_user_code: "TEST-0001",
          sweep_project: "PRIV",
          invite_url: inviteUrl,
        });
        expect(Buffer.from(inputs.sweep_fills ?? "", "base64").toString()).toContain(
          `proxy_repo\t${proxyRepository}`
        );
        // The run's artifacts read as the local command's files are.
        expect({ verdicts: verdictsOf(started.stdout), stdout: started.stdout }).toMatchObject({
          verdicts: { T1: "PASS", T4: "PASS", public: "PASS", sweep: "PASS" },
        });
        // An input the workflow does not declare: GitHub's 422, and no wait on the login only the
        // run would have approved.
        expect({ verdicts: verdictsOf(refused.stdout), stdout: refused.stdout }).toMatchObject({
          verdicts: { T1: "UNKNOWN", public: "UNKNOWN" },
        });
        expect(rowsOf(refused.stdout).get("public")?.got).toContain(
          'Unexpected inputs provided: ["edge_host"]'
        );
        expect(rowsOf(refused.stdout).get("T1")?.got).toContain("only the public leg approves");
      },
      // The six runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short", "short", "short"] })
    );

    test(
      "the full member's helper's login is revoked when a TERM ends the leg, and a failed preparation leaves no stand-in running",
      async () => {
        // A TERM after the full member's helper was approved: the leg's cleanup revokes its login.
        // The control is the answered run of the first test of this suite, which revokes it on the
        // normal path.
        const [ended] = await runStandIns({ ...passingControls, t3Kills: true });
        if (ended === undefined) throw new Error("one run, want one");
        expect({ code: ended.code, seen: ended.seen }).toMatchObject({
          code: 143,
          seen: expect.arrayContaining(["POST /api/v1/machine-logins/cred-full-0001/revoke"]),
        });
        // A preparation that throws after its stand-in Dispatch started stops it, and stops each
        // stand-in of the runs prepared before it.
        const before = liveStandInDispatches();
        await expect(
          runStandIns(passingControls, { ...passingControls, failAfterDispatch: true })
        ).rejects.toThrow("a step after the stand-in Dispatch started failed");
        expect(liveStandInDispatches()).toBe(before);
      },
      // The one run, a bounded child under the short bound; the failed preparation runs no leg.
      rowLimit("short")
    );

    test(
      "P4 reads UNKNOWN where the peer was shown the public side's ping and reply before they were sent",
      async () => {
        const [ahead, inStep] = await runStandIns(
          { ...passingControls, window: { lastAnsweredAfter: 0 }, peerAheadMs: 60_000 },
          { ...passingControls, window: { lastAnsweredAfter: 0 }, peerAheadMs: 0 }
        );
        if (ahead === undefined || inStep === undefined) throw new Error("two runs, want two");
        // The runner's clock 60 s ahead of this box's: the peer's lags read below -2 s.
        expect({ verdicts: verdictsOf(ahead.stdout), stdout: ahead.stdout }).toMatchObject({
          verdicts: { P4: "UNKNOWN" },
        });
        expect(rowsOf(ahead.stdout).get("P4")?.got).toContain(
          "before it was sent, so the runner's clock and this box's disagree"
        );
        // The control: clocks in step reach the scripted peer's own reading.
        expect(rowsOf(inStep.stdout).get("P4")?.got).toContain(
          "the scripted control peer was shown the ping"
        );
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );
  }
);

/* The browser gate (OUTSIDE_GATE=browser): the browser leg alone, run while nothing of the agents'
 * path serves. Against the stand-ins' owner commands, `dispatch` and workflow run (whose step `The
 * delisting window` the stand-in `gh` reports), every row passes and the agents' path is never
 * asked; each plant then makes the one row it aims at, and only that row's kin, not PASS. */
const browserRows = [
  "pins",
  "issue",
  "unlisted",
  "T5",
  "refusals",
  "reach",
  "sign-in",
  "projects",
  "limits",
  "grants",
  "public",
  "sweep",
  "pins-end",
  "gate",
];

/** The browser gate's run against stand-ins with `browser`'s plants. */
function browserRun(browser: StandIns["browser"]): StandIns {
  return { ...passingControls, browser };
}

describe.skipIf(withoutTools)("the browser gate's refusals with no input", () => {
  test(
    "the browser gate reads each of its rows UNKNOWN with no input, OUTSIDE_GATE of another word reads every row UNKNOWN, and the check fails on a copy that defaults to PASS",
    async () => {
      const browser = await runLeg(tailnetLeg, [], { ...noInput(), OUTSIDE_GATE: "browser" });
      expect(unknownProblems(browser, browserRows)).toEqual([]);
      const other = await runLeg(tailnetLeg, ["EXAMPLE"], {
        ...(await tailnetInputs()),
        OUTSIDE_GATE: "agents",
      });
      expect(unknownProblems(other, tailnetRows)).toEqual([]);
      expect(rowsOf(other.stdout).get("pins")?.got).toContain(
        "OUTSIDE_GATE must be all or browser, not agents"
      );
      // The control: the same check on a copy of the leg in which every UNKNOWN reads PASS.
      const passing = await runLeg(passingCopy(tailnetLeg), [], {
        ...noInput(),
        OUTSIDE_GATE: "browser",
      });
      const problems = unknownProblems(passing, browserRows);
      expect(problems).toContain("exit 0, want 2");
      expect(problems.some((problem) => problem.endsWith("reads PASS, want UNKNOWN"))).toBe(true);
    },
    // The three legs, one after another, each a bounded child under the short bound.
    rowLimit("short", "short", "short")
  );
});

describe.skipIf(skipWithout("bash", "curl", "git", "base64", "bun"))(
  "the browser gate against stand-ins",
  () => {
    test(
      "every row passes on the owner commands, the workflow's checks and its window step, with no edge listener pin, no agents' input and no owner's browser session; the same stand-ins under OUTSIDE_GATE=all read the edge listener's pin",
      async () => {
        const [run] = await runStandIns(browserRun({}));
        if (run === undefined) throw new Error("one run, want one");
        const prepared = await prepareStandIn(scratch, browserRun({}));
        const control = await runStandIn({
          ...prepared,
          env: { ...prepared.env, OUTSIDE_GATE: "all" },
        });
        const rows = rowsOf(run.stdout);
        // Every row PASS, in the gate's order, and the gate's exit 0.
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: Object.fromEntries(browserRows.map((name) => [name, "PASS"])),
        });
        expect([...rows.keys()]).toEqual(browserRows);
        expect(run.code).toBe(0);
        // The pins of the broker and Dispatch alone, though the pins file names an edge listener
        // whose file the deployment repository lacks.
        expect(rows.get("pins")?.got).not.toContain("edge-listener");
        expect(rows.get("unlisted")?.got).toContain("refused 409 PERSON_NOT_LISTED");
        expect(rows.get("T5")?.got).toContain("T5-cookie at at most");
        expect(rows.get("T5")?.got).toContain("T5-passkey at at most");
        // The workflow got the browser checks, Dispatch's routed control, the owner's one link
        // (not the link OUTSIDE_WORKFLOW_INPUTS held), the issue row's key and the probe grant,
        // the one grant of the full member's list with no approver and no operator, on stdin alone.
        const links = readFileSync(join(run.state, "links"), "utf8").trim().split("\n");
        expect(links).toHaveLength(1);
        const inputs = JSON.parse(
          readFileSync(join(run.state, "workflow-inputs.json"), "utf8")
        ) as Record<string, string>;
        expect(inputs).toEqual({
          checks: "browser",
          project: "EXAMPLE",
          routed_controls: "dispatch-healthz",
          invite_url: links[0],
          sweep_member_issue: "EXAMPLE-7",
          grants_probe_id: serviceGrant,
          sweep_project: "PRIV",
          sweep_fills: expect.any(String),
        });
        // The full member's list was read before the run and after it, with the token.
        expect(rows.get("grants")?.got).toContain(
          "the full member's list held 1 grants every signed-in person is listed before the run and 1 after, 1 in both"
        );
        expect(run.seen.filter((each) => each === "GET /api/v1/credential-grants")).toHaveLength(2);
        expect(readFileSync(join(run.state, "workflow-args"), "utf8")).not.toContain(linkToken);
        expect(`${run.stdout}${run.stderr}`).not.toContain(linkToken);
        // The window was read from the step's status; the owner's remove took the person off.
        expect(existsSync(join(run.state, "window-reads"))).toBe(true);
        expect(readFileSync(join(run.state, "listed"), "utf8")).not.toContain(
          "outside-e2e@example.com"
        );
        // No agents' path: no agent-secrets call, no full member's helper, and the access list
        // read by the owner's command, never the full member's browser.
        expect(existsSync(join(run.state, "agent-secrets-calls"))).toBe(false);
        expect(run.seen).not.toContain("POST /api/v1/me/agent-tokens");
        expect(run.seen).not.toContain("GET /api/v1/projects/EXAMPLE/access");
        // The control: under OUTSIDE_GATE=all the edge listener's pin is read, and holds the gate.
        expect(rowsOf(control.stdout).get("pins")).toMatchObject({ verdict: "UNKNOWN" });
        expect(rowsOf(control.stdout).get("pins")?.got).toContain("edge-listener");
      },
      // Two preparations, then the control run and the browser run one after another, each a
      // bounded child under the short bound.
      rowLimit("short", "short")
    );

    test(
      "the owner rows: a token refused 403 BROWSER_SESSION_REQUIRED starts no run, an address already listed gets a link and fails unlisted, a remove that removes nobody holds T5, and OUTSIDE_PUBLIC_COMMAND is refused",
      async () => {
        const [session, listed, kept, local] = await runStandIns(
          browserRun({ invite: "browser-session" }),
          browserRun({ unlistedListed: true }),
          browserRun({ removeNothing: true }),
          browserRun({ publicCommand: true })
        );
        if (
          session === undefined ||
          listed === undefined ||
          kept === undefined ||
          local === undefined
        ) {
          throw new Error("four runs, want four");
        }
        // Today's Dispatch answers an owner's token on the invite route 403
        // BROWSER_SESSION_REQUIRED: no link, so no run, and nothing reads PASS from it.
        expect({ verdicts: verdictsOf(session.stdout), stdout: session.stdout }).toMatchObject({
          verdicts: {
            unlisted: "UNKNOWN",
            T5: "UNKNOWN",
            refusals: "UNKNOWN",
            "sign-in": "UNKNOWN",
            public: "UNKNOWN",
            sweep: "UNKNOWN",
          },
        });
        expect(rowsOf(session.stdout).get("unlisted")?.got).toContain(
          "refused 403 BROWSER_SESSION_REQUIRED"
        );
        expect(rowsOf(session.stdout).get("public")?.got).toContain("no invitation link");
        expect(existsSync(join(session.state, "workflow-args"))).toBe(false);
        expect(session.code).toBe(2);
        // An address on the list gets its link: unlisted FAILs, and prints no link.
        expect({ verdicts: verdictsOf(listed.stdout), stdout: listed.stdout }).toMatchObject({
          verdicts: { unlisted: "FAIL", T5: "PASS", public: "PASS" },
        });
        expect(rowsOf(listed.stdout).get("unlisted")?.got).toContain(
          `made a link for ${unlistedPerson}`
        );
        expect(readFileSync(join(listed.state, "links"), "utf8").trim().split("\n")).toHaveLength(
          2
        );
        expect(`${listed.stdout}${listed.stderr}`).not.toContain(linkToken);
        expect(listed.code).toBe(1);
        // The run's own T5 rows pass, yet the list still names the person: no removal was made.
        expect({ verdicts: verdictsOf(kept.stdout), stdout: kept.stdout }).toMatchObject({
          verdicts: { T5: "UNKNOWN", unlisted: "PASS", public: "PASS" },
        });
        expect(rowsOf(kept.stdout).get("T5")?.got).toContain("the access list reads true");
        expect(kept.code).toBe(2);
        // A local public command cannot open the browser gate's window: no run starts.
        expect({ verdicts: verdictsOf(local.stdout), stdout: local.stdout }).toMatchObject({
          verdicts: { T5: "UNKNOWN", public: "UNKNOWN", refusals: "UNKNOWN" },
        });
        expect(rowsOf(local.stdout).get("public")?.got).toContain("OUTSIDE_PUBLIC_COMMAND");
        expect(existsSync(join(local.state, "workflow-args"))).toBe(false);
      },
      // The four runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short"] })
    );

    test(
      "with no full member's cookie the sweep's proxy reads use the full member's token, and only a build that refuses it holds the sweep row, while the owner's rows pass on their commands",
      async () => {
        const [noCookie, refused] = await runStandIns(
          { ...browserRun({}), cookie: "absent" },
          { ...browserRun({}), cookie: "absent", proxyRefusesToken: true }
        );
        if (noCookie === undefined || refused === undefined) throw new Error("two runs, want two");
        // Where Dispatch takes the token on its GitHub proxy, no cookie is needed: the fills and
        // the box's proxy control read with the token, and every row passes.
        expect({ verdicts: verdictsOf(noCookie.stdout), stdout: noCookie.stdout }).toMatchObject({
          verdicts: { sweep: "PASS", unlisted: "PASS", T5: "PASS", gate: "PASS" },
        });
        expect(existsSync(join(noCookie.state, "fills-read-the-token"))).toBe(true);
        expect(noCookie.code).toBe(0);
        // A build whose proxy reads a browser's person alone refuses the token: the fills go to
        // the workflow with no proxy read's line, and the sweep row, and so the gate, reads
        // UNKNOWN naming that refusal, never PASS.
        expect({ verdicts: verdictsOf(refused.stdout), stdout: refused.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN", unlisted: "PASS", T5: "PASS", gate: "UNKNOWN" },
        });
        expect(rowsOf(refused.stdout).get("sweep")?.got).toContain(
          "refused 403 OAUTH_SCOPE_REQUIRED: this build of Dispatch reads the proxy as a browser's person alone"
        );
        expect(existsSync(join(refused.state, "fills-without-cookie"))).toBe(true);
        expect(existsSync(join(refused.state, "writer-stopped"))).toBe(true);
        const fills = (
          JSON.parse(readFileSync(join(refused.state, "workflow-inputs.json"), "utf8")) as Record<
            string,
            string
          >
        ).sweep_fills;
        expect(Buffer.from(fills ?? "", "base64").toString()).toContain(
          `proxy_repo\t${proxyRepository}`
        );
        expect(Buffer.from(fills ?? "", "base64").toString()).not.toContain("can_fail");
        expect(refused.code).toBe(2);
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "the grants row passes on the runner's two rows and a control both reads hold, fails on either row's FAIL with or without the control, and reads UNKNOWN on an empty control, a probe grant that names an approver or an operator, or no artifact",
      async () => {
        const [listed, revokedBare, bare, named, guarded, absent] = await runStandIns(
          browserRun({ grantRows: ["FAIL", "PASS"] }),
          browserRun({ grantRows: ["PASS", "FAIL"], fullGrants: "no-everyone" }),
          browserRun({ fullGrants: "no-everyone" }),
          browserRun({ fullGrants: "judged-only" }),
          browserRun({ grantRows: ["PASS", "UNKNOWN"] }),
          browserRun({ grantRows: "none" })
        );
        if (
          listed === undefined ||
          revokedBare === undefined ||
          bare === undefined ||
          named === undefined ||
          guarded === undefined ||
          absent === undefined
        ) {
          throw new Error("six runs, want six");
        }
        const probeOf = (run: StandInRun) =>
          (
            JSON.parse(readFileSync(join(run.state, "workflow-inputs.json"), "utf8")) as Record<
              string,
              string
            >
          ).grants_probe_id;
        // Another session's grant on the person's list is a FAIL by itself.
        expect({ verdicts: verdictsOf(listed.stdout), stdout: listed.stdout }).toMatchObject({
          verdicts: { grants: "FAIL", limits: "PASS" },
        });
        expect(listed.code).toBe(1);
        // A revoke let through fails though no grant every signed-in person is listed was there,
        // and with no grant of no approver and no operator, no probe grant is passed at all.
        expect(verdictsOf(revokedBare.stdout)).toMatchObject({ grants: "FAIL" });
        expect(probeOf(revokedBare)).toBeUndefined();
        // An empty list proves nothing where nothing was there to list.
        expect(verdictsOf(bare.stdout)).toMatchObject({ grants: "UNKNOWN" });
        expect(rowsOf(bare.stdout).get("grants")?.got).toContain(
          "0 grants every signed-in person is listed before the run and 0 after, 0 in both"
        );
        // A list whose only grant with no approver names the full member as its operator, beside
        // one the model judge approved for anyone: the control holds, and no probe grant is given.
        expect(verdictsOf(named.stdout)).toMatchObject({ grants: "UNKNOWN" });
        expect(rowsOf(named.stdout).get("grants")?.got).toContain(
          "no grant of the list before the run had no approver and no operator, so no probe grant was given"
        );
        expect(probeOf(named)).toBeUndefined();
        // The driver's own guard, a probe grant its list shows naming an approver or an operator,
        // makes no revoke and reads UNKNOWN.
        expect(verdictsOf(guarded.stdout)).toMatchObject({ grants: "UNKNOWN" });
        // No artifact is no PASS.
        expect(verdictsOf(absent.stdout)).toMatchObject({ grants: "UNKNOWN" });
        expect(rowsOf(absent.stdout).get("grants")?.got).toContain(
          "the run left no outside-edge-grants.tsv"
        );
      },
      // The six runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short", "short", "short"] })
    );

    test(
      "the owner's acts by the owner's browser session, with the called origin's Origin: every row passes with no owner command; a write sent another Origin is refused 403 invalid request origin, which the rows name; and with neither, the rows read UNKNOWN naming both",
      async () => {
        const [byCookie, otherOrigin, neither] = await runStandIns(
          browserRun({ ownerByCookie: true }),
          {
            ...browserRun({ ownerByCookie: true }),
            legChange: (text) =>
              text.replace(
                '-H "Origin: $OUTSIDE_TAILNET_DISPATCH_URL"',
                '-H "Origin: https://elsewhere.example"'
              ),
          },
          { ...browserRun({ ownerByCookie: true }), cookie: "absent" }
        );
        if (byCookie === undefined || otherOrigin === undefined || neither === undefined) {
          throw new Error("three runs, want three");
        }
        expect({ verdicts: verdictsOf(byCookie.stdout), stdout: byCookie.stdout }).toMatchObject({
          verdicts: Object.fromEntries(browserRows.map((name) => [name, "PASS"])),
        });
        expect(byCookie.seen).toEqual(
          expect.arrayContaining([
            "PUT /api/v1/projects/EXAMPLE/access/people/outside-e2e@example.com",
            "GET /api/v1/projects/EXAMPLE/access",
            "POST /api/v1/projects/EXAMPLE/access/people/outside-e2e@example.com/invitations",
            "DELETE /api/v1/projects/EXAMPLE/access/people/outside-e2e@example.com",
          ])
        );
        expect(rowsOf(byCookie.stdout).get("unlisted")?.got).toContain(
          "refused 409 PERSON_NOT_LISTED"
        );
        expect(`${byCookie.stdout}${byCookie.stderr}`).not.toContain(linkToken);
        // The control: the same acts sent another Origin are refused, and no run starts.
        expect({
          verdicts: verdictsOf(otherOrigin.stdout),
          stdout: otherOrigin.stdout,
        }).toMatchObject({ verdicts: { unlisted: "UNKNOWN", public: "UNKNOWN", T5: "UNKNOWN" } });
        expect(rowsOf(otherOrigin.stdout).get("unlisted")?.got).toContain(
          "refused 403 invalid request origin"
        );
        expect(rowsOf(otherOrigin.stdout).get("public")?.got).toContain(
          "exited 1 (403 invalid request origin)"
        );
        expect(existsSync(join(otherOrigin.state, "workflow-args"))).toBe(false);
        // Neither a command nor the session: each owner row names both.
        expect(verdictsOf(neither.stdout)).toMatchObject({
          unlisted: "UNKNOWN",
          public: "UNKNOWN",
        });
        expect(rowsOf(neither.stdout).get("unlisted")?.got).toContain(
          "neither OUTSIDE_OWNER_INVITE_COMMAND nor the owner's browser session OUTSIDE_FULL_COOKIE_FILE is given"
        );
        expect(existsSync(join(neither.state, "workflow-args"))).toBe(false);
      },
      // The three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "outside-agent-grants.sh run alone prints the probe grant, `no grant with no approver and no operator` (exit 1), or UNKNOWN without the token (exit 2)",
      async () => {
        const prepared = await Promise.all(
          (["default", "judged-only"] as const).map((fullGrants) =>
            prepareStandIn(scratch, browserRun({ fullGrants }))
          )
        );
        try {
          const preflight = (each: PreparedStandIn, env: Record<string, string> = {}) =>
            runLeg(join(each.leg, "..", "outside-agent-grants.sh"), [], {
              PATH: each.env.PATH ?? "",
              HOME: each.env.HOME ?? "",
              OUTSIDE_ALLOW_HTTP: "1",
              OUTSIDE_TAILNET_DISPATCH_URL: each.dispatch.origin,
              OUTSIDE_SWEEP_FULL_TOKEN_FILE: each.env.OUTSIDE_SWEEP_FULL_TOKEN_FILE ?? "",
              OUTSIDE_PERSON: "outside-e2e@example.com",
              ...env,
            });
          const [found, none] = prepared;
          if (found === undefined || none === undefined) throw new Error("two stand-ins, want two");
          const [grant, noGrant, noToken] = await Promise.all([
            preflight(found),
            preflight(none),
            preflight(found, { OUTSIDE_SWEEP_FULL_TOKEN_FILE: "" }),
          ]);
          expect({ code: grant.code, stdout: grant.stdout }).toEqual({
            code: 0,
            stdout: `grant ${serviceGrant}\n`,
          });
          expect({ code: noGrant.code, stdout: noGrant.stdout }).toEqual({
            code: 1,
            stdout: "no grant with no approver and no operator\n",
          });
          expect(noToken.code).toBe(2);
          expect(noToken.stdout).toStartWith("UNKNOWN: OUTSIDE_SWEEP_FULL_TOKEN_FILE is unset");
        } finally {
          for (const each of prepared) each.dispatch.stop();
        }
      },
      // Two preparations, then the three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "the workflow rows: a skipped window step, a late cookie, a check's FAIL, a check's missing exit and the driver's limits each hold their own row",
      async () => {
        const [skipped, late, failed, uncoded, overLimit, noLimit] = await runStandIns(
          browserRun({ noWindow: true }),
          browserRun({ cookieAfter: 60 }),
          browserRun({ codes: { "outside-edge-refusals": 1 } }),
          browserRun({ noCode: ["outside-path-reach"] }),
          browserRun({ limits: ["PASS", "FAIL"] }),
          browserRun({ limits: [] })
        );
        if (
          skipped === undefined ||
          late === undefined ||
          failed === undefined ||
          uncoded === undefined ||
          overLimit === undefined ||
          noLimit === undefined
        ) {
          throw new Error("six runs, want six");
        }
        // A step that never ran: no removal, and T5 UNKNOWN.
        expect({ verdicts: verdictsOf(skipped.stdout), stdout: skipped.stdout }).toMatchObject({
          verdicts: { T5: "UNKNOWN", refusals: "PASS" },
        });
        expect(rowsOf(skipped.stdout).get("T5")?.got).toContain("before this leg read");
        expect(existsSync(join(skipped.state, "removed"))).toBe(false);
        // The cookie's 401 a minute after the removal is past its 30 s.
        expect({ verdicts: verdictsOf(late.stdout), stdout: late.stdout }).toMatchObject({
          verdicts: { T5: "FAIL" },
        });
        expect(rowsOf(late.stdout).get("T5")?.got).toContain("past 30 s");
        expect(late.code).toBe(1);
        // A check's exit 1 is its row's FAIL, and the run's failure is the public row's FAIL.
        expect({ verdicts: verdictsOf(failed.stdout), stdout: failed.stdout }).toMatchObject({
          verdicts: { refusals: "FAIL", reach: "PASS", public: "FAIL" },
        });
        expect(rowsOf(failed.stdout).get("public")?.got).toContain("outside-edge-refusals 1");
        // A check whose exit the run did not keep is no PASS, whatever else it printed.
        expect({ verdicts: verdictsOf(uncoded.stdout), stdout: uncoded.stdout }).toMatchObject({
          verdicts: { reach: "UNKNOWN", refusals: "PASS" },
        });
        expect(rowsOf(uncoded.stdout).get("reach")?.got).toContain(
          "the run left no outside-path-reach.code"
        );
        expect(uncoded.code).toBe(2);
        // One limit row's FAIL fails limits; no limit row at all is no PASS.
        expect(verdictsOf(overLimit.stdout)).toMatchObject({ limits: "FAIL", "sign-in": "PASS" });
        expect(rowsOf(overLimit.stdout).get("limits")?.got).toContain("limit-2-60s");
        expect(verdictsOf(noLimit.stdout)).toMatchObject({ limits: "UNKNOWN" });
        expect(rowsOf(noLimit.stdout).get("limits")?.got).toContain("holds no limit row");
      },
      // The six runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short", "short", "short"] })
    );
  }
);
