/** The tailnet leg (`e2e/acceptance/outside-agent-tailnet.sh`, with the files it sources) prints no
 *  word of what `gh` said or answered but a value it expects. Whatever answers gh's calls in
 *  GitHub's place (a GH_HOST, a proxy) holds gh's token, and writes gh's error message, its answer,
 *  the pin files `gh api` reads and the run's artifacts `gh run download` writes, any of which can
 *  hold that token.
 *
 *  Each row runs the leg against the stand-ins of `outside-agent-tailnet-stand-ins.ts`, with a `gh`
 *  first on PATH that records each call and what the stand-in `gh` behind it said and answered, and
 *  puts the marker `not-a-secret-token` in what one of the leg's gh calls says or answers: in place
 *  of the stand-in's answer, in the pin file the stand-in serves, or in the artifacts it downloads.
 *  No run of eight of the marker's characters may be in what the leg printed, stdout and stderr
 *  together. The record of the call is the control that this `gh` is the one the leg called, and
 *  the twin: what it said or answered does hold the marker. Each row also reads the leg's verdicts,
 *  which keep what they were, and the words that name the file the leg kept gh's words in, which
 *  holds the marker. */

import { afterEach, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { memoryRoot, temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";
import { type LegRun, rowsOf, script, verdictsOf } from "./outside-agent-fixture";
import {
  type BrowserStandIns,
  ghKeptDirectory,
  type PreparedStandIn,
  passingControls,
  prepareStandIn,
  type StandIns,
} from "./outside-agent-tailnet-stand-ins";

const marker = "not-a-secret-token";

// On tmpfs, as outside-agent-acceptance.test.ts's own scratch is, for the same reason: a row's
// directories are removed by an afterEach that has Bun's 5 s.
const scratch = temporaryDirectories("outside-agent-gh-words", {
  root: memoryRoot({ need: 8 * 2 ** 20 }),
});
// The directory each run of the leg kept gh's words in, which the leg leaves in place.
const ghKept: string[] = [];
afterEach(() => {
  for (const each of ghKept.splice(0)) rmSync(each, { recursive: true, force: true });
});

/** Each line of `text` that holds a run of eight characters of the marker, as its first field (a
 *  TSV row's name) and the words around the first such run, so that a failure names the row and
 *  shows the run however long the row is. */
function markerLines(text: string): string[] {
  const pieces = Array.from({ length: marker.length - 7 }, (_, at) => marker.slice(at, at + 8));
  return text.split("\n").flatMap((line) => {
    const at = Math.min(
      ...pieces.map((piece) => line.indexOf(piece)).filter((found) => found >= 0)
    );
    if (!Number.isFinite(at)) return [];
    return [`${line.split("\t")[0]}: ...${line.slice(Math.max(0, at - 100), at + 40)}...`];
  });
}

/** What the `gh` first on PATH plants. */
interface Plant {
  /** Its answer, in place of the stand-in's, to a call whose words hold ` <word> `, `[word,
   *  answer]`. With `jump`, an answer also moves the leg's clock (`date -u +%s`) 4000 s on, past
   *  the hour a wait of the leg is bounded by. */
  readonly answers?: readonly (readonly [string, string])[];
  readonly jump?: boolean;
  /** Text replaced in the artifacts `gh run download` writes, `[file, from, to]`. */
  readonly artifacts?: readonly (readonly [string, string, string])[];
  /** The image dispatch.yaml at the deployment repository's main names at the first read, and at
   *  every read after it. */
  readonly pin?: readonly [string, string];
  /** More of the leg's environment. */
  readonly env?: Readonly<Record<string, string>>;
}

interface PlantedRun extends LegRun {
  /** The words of each gh call the leg made, in order. */
  readonly calls: readonly string[];
  /** What the stand-in gh said on stderr, and answered on stdout, over every call. */
  readonly said: string;
  readonly answered: string;
  /** The artifacts as gh downloaded them, each file's last copy. */
  readonly downloads: string;
  /** The directory the leg kept gh's words in, as its stderr names it. */
  readonly kept: string | undefined;
}

/** Applies the artifact replacements to each file of a download, and keeps a copy of each. */
const rewrite = `import { copyFileSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
const [dir, rules, keep] = process.argv.slice(2) as [string, string, string];
const replacements = JSON.parse(readFileSync(rules, "utf8")) as [string, string, string][];
for (const name of readdirSync(dir)) {
  let text = readFileSync(join(dir, name), "utf8");
  for (const [file, from, to] of replacements) if (file === name) text = text.replaceAll(from, to);
  writeFileSync(join(dir, name), text);
  copyFileSync(join(dir, name), join(keep, name));
}
`;

/** The planting `gh`: records the call, answers it where `plant.answers` names it, and otherwise
 *  runs the stand-in `gh` and hands on what it said and answered, the artifacts a download wrote
 *  replaced as `plant.artifacts` says. */
function plantedGh(dir: string, realGh: string, state: string, plant: Plant): string {
  const answers = (plant.answers ?? [])
    .map(([word, answer]) => {
      if (`${word}${answer}`.includes("'")) throw new Error("a planted answer holds a quote");
      return `  *' ${word} '*)
    printf '%s\\n' '${answer}' >>"$plant/answers"
    ${plant.jump === true ? ': >"$plant/jump"' : ":"}
    printf '%s\\n' '${answer}'
    exit 0
    ;;`;
    })
    .join("\n");
  return `#!/usr/bin/env bash
plant='${dir}'
printf '%s\\n' "$*" >>"$plant/calls"
case " $* " in
${answers}
  *'/contents/dispatch.yaml?ref=main '*)
    [ ! -f "$plant/pin-read" ] || [ ! -f "$plant/pin-later" ] || cp "$plant/pin-later" '${state}/deploy/dispatch.yaml'
    : >"$plant/pin-read"
    ;;
esac
'${realGh}' "$@" >"$plant/out.$$" 2>"$plant/err.$$"
status=$?
if [ "$1 $2" = 'run download' ]; then
  dir=''
  while [ "$#" -gt 0 ]; do
    case $1 in -D) dir=$2 && shift ;; esac
    shift
  done
  '${process.execPath}' "$plant/rewrite.ts" "$dir" "$plant/artifacts.json" "$plant/downloads"
fi
cat "$plant/out.$$" >>"$plant/answers"
cat "$plant/err.$$" >>"$plant/said"
cat "$plant/out.$$"
cat "$plant/err.$$" >&2
rm -f "$plant/out.$$" "$plant/err.$$"
exit "$status"
`;
}

/** A \`date\` that answers \`date -u +%s\` 4000 s on once the planting \`gh\` has answered a planted
 *  call, and is the host's \`date\` otherwise. */
function jumpingDate(dir: string): string {
  const date = hostTool("date");
  if (date === null) throw new Error("this host has no date");
  return `#!/usr/bin/env bash
if [ "$*" = '-u +%s' ] && [ -f '${dir}/jump' ]; then
  echo "$(( $('${date}' -u +%s) + 4000 ))"
else
  exec '${date}' "$@"
fi
`;
}

interface Planted {
  readonly prepared: PreparedStandIn;
  readonly dir: string;
  readonly env: Record<string, string>;
}

/** The stand-ins ready to run, with the planting `gh` (and, for `jump`, the jumping `date`) first
 *  on PATH. */
async function plantStandIn(standIns: StandIns, plant: Plant): Promise<Planted> {
  const prepared = await prepareStandIn(scratch, standIns);
  try {
    const dir = scratch("plant");
    const bin = join(dir, "bin");
    mkdirSync(bin);
    mkdirSync(join(dir, "downloads"));
    const path = prepared.env.PATH ?? "";
    const realGh = join(path, "gh");
    if (!existsSync(realGh)) throw new Error(`the stand-in gh is not at ${realGh}`);
    if (plant.pin !== undefined) {
      writeFileSync(
        join(prepared.state, "deploy", "dispatch.yaml"),
        `example:image: ${plant.pin[0]}\n`
      );
      writeFileSync(join(dir, "pin-later"), `example:image: ${plant.pin[1]}\n`);
    }
    writeFileSync(join(dir, "artifacts.json"), JSON.stringify(plant.artifacts ?? []));
    writeFileSync(join(dir, "rewrite.ts"), rewrite);
    script(join(bin, "gh"), plantedGh(dir, realGh, prepared.state, plant));
    if (plant.jump === true) script(join(bin, "date"), jumpingDate(dir));
    return { prepared, dir, env: { ...prepared.env, ...plant.env, PATH: `${bin}:${path}` } };
  } catch (error) {
    prepared.dispatch.stop();
    throw error;
  }
}

/** Prepares each run, one at a time, then runs every one at once; every run answers, in the order
 *  given. A preparation that throws stops the stand-in Dispatch of each run prepared before it. */
async function plantedRuns(...runs: (readonly [StandIns, Plant])[]): Promise<PlantedRun[]> {
  const planted: Planted[] = [];
  try {
    for (const [standIns, plant] of runs) planted.push(await plantStandIn(standIns, plant));
  } catch (error) {
    for (const done of planted) done.prepared.dispatch.stop();
    throw error;
  }
  return Promise.all(
    planted.map(async ({ prepared, dir, env }) => {
      try {
        const { code, stdout, stderr } = await runBounded({
          argv: ["bash", prepared.leg, "EXAMPLE"],
          bound: "short",
          cwd: scratch("cwd"),
          env,
          from: import.meta.url,
        });
        const kept = ghKeptDirectory(stderr);
        if (kept !== undefined) ghKept.push(kept);
        const read = (name: string) =>
          existsSync(join(dir, name)) ? readFileSync(join(dir, name), "utf8") : "";
        return {
          code,
          stdout,
          stderr,
          calls: read("calls")
            .split("\n")
            .filter((line) => line !== ""),
          said: read("said"),
          answered: read("answers"),
          downloads: join(dir, "downloads"),
          kept,
        };
      } finally {
        prepared.dispatch.stop();
      }
    })
  );
}

/** The text of every file in `dir`. */
function filesOf(dir: string): string {
  return readdirSync(dir)
    .map((name) => readFileSync(join(dir, name), "utf8"))
    .join("\n");
}

/** The row's evidence, in this order: the control, a call of the planting `gh` whose words hold
 *  `call`; the twin, `twin` (what that `gh` said, answered or downloaded) holding the marker; and
 *  no run of eight of the marker's characters in what the leg printed. */
function expectNoMarker(run: PlantedRun, call: string, twin: string): void {
  expect({ called: run.calls.some((each) => each.includes(call)), calls: run.calls }).toMatchObject(
    { called: true }
  );
  expect(markerLines(twin)).not.toEqual([]);
  expect(markerLines(`${run.stdout}\n${run.stderr}`)).toEqual([]);
}

/** The words that name a file of the artifacts gh downloaded. */
function keptArtifact(run: PlantedRun, file: string): string {
  return `kept in ${run.kept}/artifacts/${file}, which gh downloaded, so not printed`;
}

function browserRun(browser: BrowserStandIns): StandIns {
  return { ...passingControls, browser };
}

const workflowRepository = "example-owner/example-workflows";

describe.skipIf(skipWithout("bash", "curl", "git", "base64", "bun"))(
  "the tailnet leg prints no word gh said or answered",
  () => {
    test(
      "gh repo view: a visibility of none of GitHub's three is not printed, and starts no run",
      async () => {
        const [run] = await plantedRuns([
          { ...passingControls, workflow: { visibility: marker } },
          {},
        ]);
        if (run === undefined) throw new Error("one run, want one");
        // The verdicts the leg gave such an answer before: no run, and T1 and public UNKNOWN.
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: { T1: "UNKNOWN", public: "UNKNOWN" },
        });
        expect(run.calls.some((each) => each.startsWith("workflow run"))).toBe(false);
        expectNoMarker(run, `repo view ${workflowRepository} --json visibility`, run.answered);
        expect(rowsOf(run.stdout).get("public")?.got).toContain(
          `gh repo view answered a visibility of ${workflowRepository} that is none of PRIVATE, INTERNAL and PUBLIC (its answer is kept in ${run.kept}/repo-view)`
        );
        expect(readFileSync(`${run.kept}/repo-view`, "utf8")).toContain(marker);
      },
      // The one run, a bounded child under the short bound.
      rowLimit("short")
    );

    test(
      "gh workflow run: what it said when it started no run is not printed",
      async () => {
        const [run] = await plantedRuns([
          { ...passingControls, workflow: { inputs: `${marker}=example` } },
          {},
        ]);
        if (run === undefined) throw new Error("one run, want one");
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: { T1: "UNKNOWN", public: "UNKNOWN" },
        });
        expectNoMarker(
          run,
          `workflow run outside-edge-e2e.yaml -R ${workflowRepository} --json`,
          run.said
        );
        expect(rowsOf(run.stdout).get("public")?.got).toContain(
          `gh workflow run printed no run URL; what it printed is kept in ${run.kept}/workflow-run`
        );
        expect(readFileSync(`${run.kept}/workflow-run`, "utf8")).toContain(marker);
      },
      // The one run, a bounded child under the short bound.
      rowLimit("short")
    );

    test(
      "gh api: the pin file it reads is not printed but as the file that keeps it and the image's digest, in the pins row, in pins-end and where no commit is read for the image, nor a commit read for that image that is no whole commit id",
      async () => {
        const digest = "0123456789abcdef".repeat(4);
        const moved = "f".repeat(64);
        const start = `registry.example/dispatch-${marker}@sha256:${digest}`;
        const end = `registry.example/dispatch-${marker}-moved@sha256:${moved}`;
        const silent = script(join(scratch("revision"), "revision.sh"), "#!/bin/sh\n");
        // The registry an image gh's answer names is the answering party's to name, so the
        // revision read for it is that party's words too: seven hex digits, then the marker.
        const said = script(
          join(scratch("revision"), "revision.sh"),
          `#!/bin/sh\necho 0123456${marker}\n`
        );
        const [movedRun, unread, unwhole] = await plantedRuns(
          [passingControls, { pin: [start, end] }],
          [
            passingControls,
            { pin: [start, start], env: { OUTSIDE_IMAGE_REVISION_COMMAND: `sh ${silent}` } },
          ],
          [
            passingControls,
            {
              pin: [
                `registry.example/dispatch@sha256:${digest}`,
                `registry.example/dispatch@sha256:${digest}`,
              ],
              env: { OUTSIDE_IMAGE_REVISION_COMMAND: `sh ${said}` },
            },
          ]
        );
        if (movedRun === undefined || unread === undefined || unwhole === undefined) {
          throw new Error("three runs, want three");
        }
        const call = "api repos/example-owner/example-deploy/contents/dispatch.yaml?ref=main";
        // gh answers a pin file as base64.
        const decoded = (run: PlantedRun) =>
          run.answered
            .split("\n")
            .map((line) => Buffer.from(line, "base64").toString())
            .join("\n");
        // A pin that moved between the legs holds pins-end, and only it; no commit read holds pins.
        expect({ verdicts: verdictsOf(movedRun.stdout), stdout: movedRun.stdout }).toMatchObject({
          verdicts: { pins: "PASS", "pins-end": "UNKNOWN" },
        });
        for (const run of [unread, unwhole]) {
          expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { pins: "UNKNOWN", issue: "UNKNOWN" },
          });
        }
        expectNoMarker(movedRun, call, decoded(movedRun));
        expectNoMarker(unread, call, decoded(unread));
        expectNoMarker(unwhole, call, readFileSync(said, "utf8"));
        const pinned = (run: PlantedRun, read: number) =>
          `dispatch (the image in ${run.kept}/pin-dispatch.${read}, which gh read: @sha256:`;
        expect(rowsOf(movedRun.stdout).get("pins")?.got).toContain(
          `${pinned(movedRun, 1)}${digest})`
        );
        const pinsEnd = rowsOf(movedRun.stdout).get("pins-end")?.got ?? "";
        expect(pinsEnd).toContain(`a pin moved between the legs: at the start broker`);
        expect(pinsEnd).toContain(`${pinned(movedRun, 1)}${digest})`);
        expect(pinsEnd).toContain(`${pinned(movedRun, 2)}${moved})`);
        expect(readFileSync(`${movedRun.kept}/pin-dispatch.1`, "utf8")).toContain(marker);
        expect(readFileSync(`${movedRun.kept}/pin-dispatch.2`, "utf8")).toContain(marker);
        expect(rowsOf(unread.stdout).get("pins")?.got).toContain(
          `dispatch: no commit read for (the image in ${unread.kept}/pin-dispatch.1, which gh read: @sha256:${digest}) through OUTSIDE_IMAGE_REVISION_COMMAND`
        );
        expect(rowsOf(unwhole.stdout).get("pins")?.got).toContain(
          `dispatch: no commit read for (the image in ${unwhole.kept}/pin-dispatch.1, which gh read: @sha256:${digest}) through OUTSIDE_IMAGE_REVISION_COMMAND`
        );
      },
      // The three runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short", "short"] })
    );

    test(
      "gh run view --json status,jobs: a read of the window step that is no status GitHub gives is not printed, when the run ended and when the hour ran out",
      async () => {
        const [ended, hour] = await plantedRuns(
          [
            browserRun({ noWindow: true }),
            { answers: [["--json status,jobs", `completed ${marker}`]] },
          ],
          [
            browserRun({ noWindow: true }),
            { answers: [["--json status,jobs", `queued ${marker}`]], jump: true },
          ]
        );
        if (ended === undefined || hour === undefined) throw new Error("two runs, want two");
        for (const run of [ended, hour]) {
          expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
            verdicts: { T5: "UNKNOWN", refusals: "PASS" },
          });
        }
        expectNoMarker(ended, "--json status,jobs", ended.answered);
        expectNoMarker(hour, "--json status,jobs", hour.answered);
        const words = (run: PlantedRun) =>
          `an answer of no statuses GitHub gives, kept in ${run.kept}/window-read`;
        expect(rowsOf(ended.stdout).get("T5")?.got).toContain(
          `run 4242 ended before this leg read its step 'The delisting window' running (the step read ${words(ended)})`
        );
        expect(rowsOf(hour.stdout).get("T5")?.got).toContain(
          `was not read running within an hour (last read: ${words(hour)})`
        );
        for (const run of [ended, hour]) {
          expect(readFileSync(`${run.kept}/window-read`, "utf8")).toContain(marker);
        }
      },
      // The two runs at once, each a bounded child under the short bound.
      rowLimit({ atOnce: ["short", "short"] })
    );

    test(
      "gh run view --json conclusion: a conclusion that is none GitHub gives is not printed",
      async () => {
        const [run] = await plantedRuns([
          { ...passingControls, workflow: {} },
          { answers: [["--json conclusion", marker]] },
        ]);
        if (run === undefined) throw new Error("one run, want one");
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: { T1: "PASS", public: "UNKNOWN" },
        });
        expectNoMarker(run, "--json conclusion", run.answered);
        const words = `no conclusion GitHub gives, kept in ${run.kept}/run-conclusion`;
        expect(rowsOf(run.stdout).get("public")?.got).toContain(`end ${words}`);
        expect(run.stderr).toContain(`outside-agent-tailnet: run 4242: ${words}`);
        expect(readFileSync(`${run.kept}/run-conclusion`, "utf8")).toContain(marker);
      },
      // The one run, a bounded child under the short bound.
      rowLimit("short")
    );

    test(
      "gh run download, both legs: no word of the public leg's artifacts is printed but a verdict, in P1-P10, P4's nonce, T4, T5, public and sweep",
      async () => {
        const [run] = await plantedRuns([
          {
            ...passingControls,
            workflow: {},
            window: { lastAnsweredAfter: 0 },
            peerAheadMs: 0,
            verdicts: { P2: marker },
            revokeVerdicts: [marker],
            sweep: { verdict: `FAIL: ${marker}`, exit: 1 },
          },
          {
            artifacts: [
              ["outside-agent-public.tsv", "what P3 read", marker],
              ["outside-agent-public.tsv", "the row P5", marker],
              ["outside-agent-public.tsv", "nonce=example", `nonce=${marker}`],
              ["outside-agent-public.tsv", "no trade refused", marker],
              ["outside-agent-public.tsv", "as read\tPASS", `${marker}\t${marker}`],
              [
                "outside-agent-public.tsv",
                "the revoke command exited 0\tPASS",
                `the revoke command exited 0\t${marker}`,
              ],
              ["restricted-sweep.exit", "1", marker],
            ],
          },
        ]);
        if (run === undefined) throw new Error("one run, want one");
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: {
            T1: "PASS",
            P2: "UNKNOWN",
            P3: "PASS",
            P4: "UNKNOWN",
            P5: "UNKNOWN",
            T4: "UNKNOWN",
            T5: "UNKNOWN",
            public: "UNKNOWN",
            sweep: "UNKNOWN",
          },
        });
        expectNoMarker(
          run,
          `run download 4242 -R ${workflowRepository} -n outside-agent-public`,
          filesOf(run.downloads)
        );
        const rows = rowsOf(run.stdout);
        const tsv = keptArtifact(run, "outside-agent-public.tsv");
        expect(rows.get("P2")?.got).toBe(`the public TSV's verdict is ${tsv}`);
        expect(rows.get("P3")?.got).toBe(tsv);
        expect(rows.get("P4")?.got).toContain(`never held its ping, reply of nonce ${tsv}; ${tsv}`);
        expect(run.stdout).toContain(`P5\tthe public leg's row P5\tPASS as the public leg read it`);
        expect(rows.get("T4")?.got).toBe(
          `revoke-cred-test-0001 reads ${keptArtifact(run, "outside-edge-revocations.tsv")}, which is no verdict`
        );
        expect(rows.get("T5")?.got).toContain(`T5-agent (refused_at) not read: ${tsv}`);
        expect(rows.get("public")?.got).toContain(
          `verdict row ${tsv} (${tsv}), revocations row ${tsv}`
        );
        expect(rows.get("sweep")?.got).toContain(
          `the sweep exited ${keptArtifact(run, "restricted-sweep.exit")}: ${keptArtifact(run, "restricted-sweep.log")}`
        );
        expect(filesOf(join(`${run.kept}`, "artifacts"))).toContain(marker);
      },
      // The one run, a bounded child under the short bound.
      rowLimit("short")
    );

    test(
      "gh run download, the browser gate: no word of the workflow's checks' artifacts is printed but a verdict or an exit, in refusals, reach, sign-in, projects, limits, grants, T5 and public",
      async () => {
        const [run] = await plantedRuns([
          browserRun({ limits: ["PASS", marker], grantRows: [marker, marker] }),
          {
            artifacts: [
              ["outside-edge-refusals.txt", "PASS: every refusal line was refused", marker],
              ["outside-path-reach.code", "0", marker],
              [
                "outside-edge.tsv",
                "signed in at https://dispatch.edge.example as the invited person",
                marker,
              ],
              ["outside-edge.tsv", "[EXAMPLE]\tPASS", `[EXAMPLE]\t${marker}`],
              ["outside-edge.tsv", "limit-1-60s", `limit-${marker}`],
              ["outside-edge.tsv", "peak 1 in 60 s from dispatch.edge.example", marker],
              ["outside-edge-grants.tsv", "listed 0 grants", marker],
              ["outside-edge-grants.tsv", "refused 403 RESTRICTED_MEMBER", marker],
              ["outside-edge-t5.tsv", "401 at=", `${marker} at=`],
              ["outside-edge-t5.tsv", "\tPASS\nT5-passkey", "\tFAIL\nT5-passkey"],
              ["outside-edge-t5.tsv", "refused at=", `${marker} `],
            ],
          },
        ]);
        if (run === undefined) throw new Error("one run, want one");
        expect({ verdicts: verdictsOf(run.stdout), stdout: run.stdout }).toMatchObject({
          verdicts: {
            refusals: "PASS",
            reach: "UNKNOWN",
            "sign-in": "PASS",
            projects: "UNKNOWN",
            limits: "UNKNOWN",
            grants: "UNKNOWN",
            T5: "FAIL",
            public: "PASS",
          },
        });
        expectNoMarker(
          run,
          `run download 4242 -R ${workflowRepository} -n outside-edge-refusals`,
          filesOf(run.downloads)
        );
        const rows = rowsOf(run.stdout);
        const edge = keptArtifact(run, "outside-edge.tsv");
        expect(rows.get("refusals")?.got).toBe(
          `outside-edge-refusals exited 0: ${keptArtifact(run, "outside-edge-refusals.txt")}`
        );
        expect(rows.get("reach")?.got).toContain(
          `outside-path-reach.code holds ${keptArtifact(run, "outside-path-reach.code")}, which is no exit of 0, 1 or 2`
        );
        expect(rows.get("sign-in")?.got).toBe(edge);
        expect(rows.get("projects")?.got).toBe(`the driver's verdict is ${edge}`);
        expect(rows.get("limits")?.got).toContain(`limit-2-60s ${edge}: ${edge}`);
        expect(rows.get("limits")?.got).toContain(`a limit row: ${edge}`);
        const grants = keptArtifact(run, "outside-edge-grants.tsv");
        expect(rows.get("grants")?.got).toContain(
          `grants-list ${grants}: ${grants}; grants-revoke ${grants}: ${grants}`
        );
        const t5 = keptArtifact(run, "outside-edge-t5.tsv");
        expect(rows.get("T5")?.got).toContain(`T5-cookie: ${t5}`);
        expect(rows.get("T5")?.got).toContain(`T5-passkey (at) not read: ${t5}`);
        expect(rows.get("public")?.got).toContain(
          `outside-path-reach ${keptArtifact(run, "outside-path-reach.code")}`
        );
        expect(filesOf(join(`${run.kept}`, "artifacts"))).toContain(marker);
      },
      // The one run, a bounded child under the short bound.
      rowLimit("short")
    );
  }
);
