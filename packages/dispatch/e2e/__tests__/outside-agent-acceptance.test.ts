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
 *  which `reap` must leave alone; and the tailnet leg against stand-ins of what it drives, reading
 *  the rows it judges by its own controls PASS where those controls answer and not PASS where they
 *  do not. The public leg's own judgments are `outside-agent-public.test.ts`'s.
 */

import { describe, expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";
import {
  acceptance,
  closedPort,
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
  liveStandInDispatches,
  type PreparedStandIn,
  passingControls,
  prepareStandIn,
  proxyRepository,
  type StandInRun,
  type StandIns,
  unknownsCounted,
  workflowInputs,
  workflowRepository,
} from "./outside-agent-tailnet-stand-ins";

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

function publicInputs(): Record<string, string> {
  const origin = `http://127.0.0.1:${closedPort()}`;
  const inputs = scratch("inputs");
  const targets = join(inputs, "targets.txt");
  writeFileSync(targets, `hub 127.0.0.1 ${closedPort()}\n`);
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

function tailnetInputs(): Record<string, string> {
  const origin = `http://127.0.0.1:${closedPort()}`;
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
      const env = publicInputs();
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
      const env = tailnetInputs();
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
        await runLeg(passingCopy(publicLeg), [], publicInputs()),
        await runLeg(passingCopy(tailnetLeg), [], noInput()),
        await runLeg(passingCopy(tailnetLeg), ["EXAMPLE"], tailnetInputs()),
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
    for (const each of standIns) prepared.push(prepareStandIn(scratch, each));
  } catch (error) {
    for (const done of prepared) done.dispatch.stop();
    throw error;
  }
  return Promise.all(prepared.map(runStandIn));
}

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
          `the full member's browser read ${proxyRepository}'s workflow runs through the proxy: 200`
        );
        // The writer's line in its `write <UTC> <comment id> <message id> delivered` form.
        expect(pass.get("sweep")?.got).toContain(
          "the writer's write at 2026-01-01T00:00:05Z (comment cmt-1, message msg-1) was delivered"
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
      "the sweep reads UNKNOWN without the full member's cookie, and with fills that lack a proxy read's line",
      async () => {
        const [noCookie, gap] = await runStandIns(
          { ...passingControls, cookie: "absent" },
          {
            ...passingControls,
            fillsLeaveOut: "GET /api/v1/github/app-installation/{owner}/{repo}",
          }
        );
        if (noCookie === undefined || gap === undefined) throw new Error("two runs, want two");
        // Without the cookie the sweep does not run, and P7 has no positive control either.
        expect({ verdicts: verdictsOf(noCookie.stdout), stdout: noCookie.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN", P7: "UNKNOWN" },
        });
        expect(rowsOf(noCookie.stdout).get("sweep")?.got).toContain(
          "OUTSIDE_FULL_COOKIE_FILE is unset"
        );
        expect(existsSync(join(noCookie.state, "fills-read-the-cookie"))).toBe(false);
        // Fills that lack a proxy read's full-member answer hold the sweep, and only the sweep.
        expect({ verdicts: verdictsOf(gap.stdout), stdout: gap.stdout }).toMatchObject({
          verdicts: { sweep: "UNKNOWN", P7: "PASS" },
        });
        expect(rowsOf(gap.stdout).get("sweep")?.got).toContain(
          "the sweep's fills hold no full member's answer that can fail for GET /api/v1/github/app-installation/{owner}/{repo}"
        );
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "the sweep counts no write the writer did not deliver, and reads UNKNOWN for a fills file fills mode refused and for a runner's UNKNOWN",
      async () => {
        const [undelivered, oldForm, refusedFills, unlisted] = await runStandIns(
          {
            ...passingControls,
            sweep: { writer: "write 2026-01-01T00:00:05Z cmt-1 msg-1 not delivered" },
          },
          { ...passingControls, sweep: { writer: "write 2026-01-01T00:00:05Z delivered" } },
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
        // A write the full member's feed was not sent, or a line of another form, is no control.
        for (const run of [undelivered, oldForm]) {
          expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { sweep: "UNKNOWN" },
          });
          expect(rowsOf(run.stdout).get("sweep")?.got).toContain(
            "no write of the writer was delivered inside its live window"
          );
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
      "the sweep reads UNKNOWN for each refusal of a version 3 runner, and the leg sends no fills of another version",
      async () => {
        // The runner's own refusals before its first request, as #4608's cd8a041312 words them.
        const refusals = [
          `UNKNOWN: SWEEP_FILLS_FILE was made from another route index: its routes.txt's SHA-256 is ${"a".repeat(64)}, and this run's is ${"c".repeat(64)}`,
          "UNKNOWN: SWEEP_FILLS_FILE was made at another deployment: the member's own projects were created at other times there (fills bbbbbbbbbbbb, here dddddddddddd)",
          "UNKNOWN: SWEEP_LIVE_SECONDS must be at least 30 on a runner, whose live window leaves the writer's 15 s delivery bound before its feed closes: 20",
        ];
        const runs = await runStandIns(
          ...refusals.map((verdict) => ({ ...passingControls, sweep: { verdict, exit: 2 } })),
          { ...passingControls, sweep: { fillsVersion: 2 } }
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
          "the sweep beside this leg wrote fills of another version than 3"
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
      "the workflow gets only the inputs its file declares, on stdin, the sweep's project among them, only in a private repository, and a refused dispatch holds the gate at once",
      async () => {
        const [started, refused, inPublic, unread] = await runStandIns(
          { ...passingControls, workflow: {} },
          { ...passingControls, workflow: { inputs: "edge_host=dispatch.edge.example" } },
          { ...passingControls, workflow: { visibility: "PUBLIC" } },
          { ...passingControls, workflow: { visibility: "" } }
        );
        if (
          started === undefined ||
          refused === undefined ||
          inPublic === undefined ||
          unread === undefined
        ) {
          throw new Error("four runs, want four");
        }
        // A repository that is not read as PRIVATE gets no run: its inputs would be as readable as
        // it is. The control is the first run, in a PRIVATE one.
        for (const [run, said] of [
          [inPublic, "is PUBLIC, not PRIVATE"],
          [unread, "could not be read"],
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
      // The four runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short", "short"] })
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
