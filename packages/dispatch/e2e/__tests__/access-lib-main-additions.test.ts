/** How `e2e/acceptance/access-lib.sh` holds the blob driver's `main_additions`: the members main
 *  added to an answer after the commit the blob driver's base is pinned to. An entry admits an
 *  answer only where the base answers no member at its path and this build answers it with the
 *  entry's value, and nothing else differs, byte for byte, once the members at its path are taken
 *  out of this build's answer alone: a member planted beside it, the member with another value, at
 *  another path or on another route, the same text moved or repeated outside the path, or a byte a
 *  JSON value does not show, such as the order of two members, still differs. An entry is in force
 *  only while this build holds its commit and the base does not; a build without the commit fails
 *  the run, a base with it leaves the entry admitting nothing, and an entry in force that admitted
 *  no answer fails the run once its report is read.
 *
 *  Each row sources the library alone in bash, with a work directory of its own and no server.
 *  The rows of an entry's force read a repository the row writes, its commits made with
 *  `commit-tree` so no hook a machine's git configuration installs runs. */

import { expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { rowLimit, runBounded } from "./bounded-subprocess";

const library = fileURLToPath(new URL("../acceptance/access-lib.sh", import.meta.url));
const scratch = temporaryDirectories("access-lib-main-additions");

/** An entry as the driver writes one, for a commit the row chooses: the member main's b537812fb5
 *  (#3995) gave each design gate, which the driver named while its base was older than that commit. */
const holdsEntry = (commit: string) =>
  `${commit}\tGET /api/v1/issues/{key}\t.design_gates[].holds\tnull`;
const holds = holdsEntry("b537812fb53d1e42b5cfe629404aeeb38a708218");

/** The rows of this build's route index the rows read, as read_route_indexes writes them. */
const routeIndex = [
  "GET\t^/api/v1/issues/[^/]+$\t/api/v1/issues/{key}",
  "GET\t^/api/v1/issues/[^/]+/summary$\t/api/v1/issues/{key}/summary",
];

interface Judged {
  readonly differed: number;
  /** How many answers the entry admitted. */
  readonly uses: number;
  /** The library's report of the entry, and the differences it wrote. */
  readonly report: string;
}

/** Holds the base's answer `base` to this build's `head`, both HTTP 200, for `request`, with the
 *  one entry `holds` in force, or kept out of force where `inForce` is false. */
async function judged(
  base: string,
  head: string,
  { request = "GET /api/v1/issues/PUB-1", inForce = true } = {}
): Promise<Judged> {
  const work = scratch();
  mkdirSync(join(work, "answers"));
  writeFileSync(join(work, "route-index-head"), `${routeIndex.join("\n")}\n`);
  writeFileSync(
    join(work, "headers"),
    ["HTTP/1.1 200 OK", "Content-Type: application/json", "", ""].join("\r\n")
  );
  const script = `
    set -euo pipefail
    work=$1
    run_epoch=$(date +%s)
    fail() { echo "FAIL: $*" >&2; exit 1; }
    note() { echo "   $*"; }
    source "$2"
    : >"$work/ids-main.tsv"
    main_additions=("$3")
    [ "$4" = no ] || main_additions_live=(0)
    status=200
    printf '%s' "$5" >"$work/body"
    keep_answer base main
    printf '%s' "$6" >"$work/body"
    keep_answer head main
    same_answers "the row" base head '' '' "$7"
    printf '%s\\t%s\\n' "$differed" "\${main_addition_uses[0]:-0}"
    main_additions_report
    if [ -s "$work/differences" ]; then cat "$work/differences"; fi
  `;
  const run = await runBounded({
    argv: [
      "bash",
      "-c",
      script,
      "main-additions",
      work,
      library,
      holds,
      inForce ? "yes" : "no",
      base,
      head,
      request,
    ],
    bound: "short",
    from: import.meta.url,
  });
  expect(run.stderr).toBe("");
  expect(run.code).toBe(0);
  const [counts = "", ...rest] = run.stdout.split("\n");
  const [differed = "", uses = ""] = counts.split("\t");
  return { differed: Number(differed), uses: Number(uses), report: rest.join("\n") };
}

/** An issue's answer whose one design gate is `gate`, the rest as the seed's first issue reads. */
const issue = (gate: string, tail = "") =>
  `{"key":"PUB-1","design_gates":[${gate}],"referenced_by_count":4${tail}}`;
const gate = `{"step":"spec","artifact_id":"<PUB spec>"}`;
const gateHolding = (value: string) =>
  `{"step":"spec","artifact_id":"<PUB spec>","holds":${value}}`;

test(
  "a member main added, absent at the base and the entry's value here, is the same answer, counted for its entry",
  async () => {
    const answer = await judged(issue(gate), issue(gateHolding("null")));
    expect(answer).toMatchObject({ differed: 0, uses: 1 });
    expect(answer.report).toContain(
      "main addition admitted 1 answers: b537812fb53d1e42b5cfe629404aeeb38a708218 GET /api/v1/issues/{key} .design_gates[].holds"
    );
    expect(answer.report).toContain("the row");
  },
  rowLimit("short")
);

test(
  "the member in every entry of the list, and first or alone in its object, is admitted too",
  async () => {
    const two = await judged(
      issue(`${gate},{"step":"criteria","artifact_id":"<PUB criteria>"}`),
      issue(
        `${gateHolding("null")},{"step":"criteria","artifact_id":"<PUB criteria>","holds":null}`
      )
    );
    expect(two).toMatchObject({ differed: 0, uses: 1 });
    const first = await judged(issue(`{"step":"spec"}`), issue(`{"holds":null,"step":"spec"}`));
    expect(first).toMatchObject({ differed: 0, uses: 1 });
    const alone = await judged(issue("{}"), issue(`{"holds":null}`));
    expect(alone).toMatchObject({ differed: 0, uses: 1 });
  },
  rowLimit("short", "short", "short")
);

test(
  "a member planted beside the one main added differs, and the entry admits nothing",
  async () => {
    const planted = `{"step":"spec","artifact_id":"<PUB spec>","holds":null,"planted":null}`;
    const answer = await judged(issue(gate), issue(planted));
    expect(answer).toMatchObject({ differed: 1, uses: 0 });
    expect(answer.report).toContain("the row (HTTP 200): the answers differ at byte");
    expect(answer.report).toContain("main addition admitted no answer");
  },
  rowLimit("short")
);

test(
  "the member with another value than the entry's differs",
  async () => {
    expect(await judged(issue(gate), issue(gateHolding("false")))).toMatchObject({
      differed: 1,
      uses: 0,
    });
  },
  rowLimit("short")
);

test(
  "the member also at a path the entry does not name differs",
  async () => {
    expect(await judged(issue(gate), issue(gateHolding("null"), `,"holds":null`))).toMatchObject({
      differed: 1,
      uses: 0,
    });
  },
  rowLimit("short")
);

test(
  "the member outside the path in both answers is left as it is, and the one main added is admitted",
  async () => {
    // Taking the text out of both answers, or out of this build's wherever it stands, would part
    // these two: only the member at the path is taken out, and of this build's answer alone.
    const outside = `,"x":{"holds":null}`;
    expect(await judged(issue(gate, outside), issue(gateHolding("null"), outside))).toMatchObject({
      differed: 0,
      uses: 1,
    });
  },
  rowLimit("short")
);

test(
  "the member moved or repeated outside the path differs, though each pair is one JSON value",
  async () => {
    const moved = await judged(
      issue(gate, `,"x":{"a":1,"holds":null}`),
      issue(gateHolding("null"), `,"x":{"holds":null,"a":1}`)
    );
    expect(moved).toMatchObject({ differed: 1, uses: 0 });
    const repeated = await judged(
      issue(gate, `,"x":{"holds":null}`),
      issue(gateHolding("null"), `,"x":{"holds":null,"holds":null}`)
    );
    expect(repeated).toMatchObject({ differed: 1, uses: 0 });
  },
  rowLimit("short", "short")
);

test(
  "a base that answers the member too is no admission of another difference",
  async () => {
    expect(
      await judged(issue(gateHolding("null")), issue(gateHolding("null"), `,"more":1`))
    ).toMatchObject({ differed: 1, uses: 0 });
  },
  rowLimit("short")
);

test(
  "members in another order beside the member main added differ, though they are one JSON value",
  async () => {
    const reordered = `{"artifact_id":"<PUB spec>","step":"spec","holds":null}`;
    expect(await judged(issue(gate), issue(reordered))).toMatchObject({ differed: 1, uses: 0 });
  },
  rowLimit("short")
);

test(
  "the member on a route the entry does not name differs",
  async () => {
    expect(
      await judged(issue(gate), issue(gateHolding("null")), {
        request: "GET /api/v1/issues/PUB-1/summary",
      })
    ).toMatchObject({ differed: 1, uses: 0 });
  },
  rowLimit("short")
);

test(
  "an entry not in force admits nothing",
  async () => {
    const answer = await judged(issue(gate), issue(gateHolding("null")), { inForce: false });
    expect(answer).toMatchObject({ differed: 1, uses: 0 });
    expect(answer.report).not.toContain("main addition admitted");
  },
  rowLimit("short")
);

test(
  "an answer that is no JSON is never admitted",
  async () => {
    expect(
      await judged(
        `{"took_ms":<duration>,${gate}}`,
        `{"took_ms":<duration>,${gateHolding("null")}}`
      )
    ).toMatchObject({ differed: 1, uses: 0 });
  },
  rowLimit("short")
);

test(
  "an entry in force that admitted no answer fails the run once its report is read",
  async () => {
    const work = scratch();
    const script = `
      set -euo pipefail
      fail() { echo "FAIL: $*" >&2; exit 1; }
      note() { echo "   $*"; }
      source "$1"
      main_additions=("$2")
      main_additions_live=(0)
      main_additions_report
      main_additions_used
      echo "the run went on"
    `;
    const run = await runBounded({
      argv: ["bash", "-c", script, "main-additions", library, holds],
      bound: "short",
      cwd: work,
      from: import.meta.url,
    });
    expect(run.code).toBe(1);
    expect(run.stdout).toContain("main addition admitted no answer");
    expect(run.stdout).not.toContain("the run went on");
    expect(run.stderr).toContain(
      "FAIL: main additions in force admitted no answer, so each names a change no read met"
    );
    expect(run.stderr).toContain(
      "b537812fb53d1e42b5cfe629404aeeb38a708218 GET /api/v1/issues/{key} .design_gates[].holds"
    );
  },
  rowLimit("short")
);

/** A repository the force rows read: main's root, the base; main's change on it; this build on the
 *  change; and a commit beside the change that this build does not hold. */
function repository(): { directory: string; root: string; change: string; beside: string } {
  const directory = scratch();
  let clock = 1_700_000_000;
  const git = (...args: string[]) => {
    clock += 60;
    const date = `${clock} +0000`;
    const run = Bun.spawnSync(["git", "-C", directory, ...args], {
      env: {
        GIT_AUTHOR_NAME: "test",
        GIT_AUTHOR_EMAIL: "test@example.com",
        GIT_AUTHOR_DATE: date,
        GIT_COMMITTER_NAME: "test",
        GIT_COMMITTER_EMAIL: "test@example.com",
        GIT_COMMITTER_DATE: date,
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
        PATH: process.env.PATH ?? "",
      },
    });
    if (run.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${run.stderr.toString()}`);
    return run.stdout.toString().trim();
  };
  git("init", "-q", "-b", "main");
  const tree = git("write-tree");
  const root = git("commit-tree", tree, "-m", "main's root");
  const change = git("commit-tree", tree, "-p", root, "-m", "main adds a member");
  const head = git("commit-tree", tree, "-p", change, "-m", "this build merges main");
  const beside = git("commit-tree", tree, "-p", root, "-m", "a commit this build does not hold");
  git("update-ref", "--no-deref", "HEAD", head);
  return { directory, root, change, beside };
}

/** main_additions_in_force over `entry`, against the base `base`, in the repository `directory`. */
async function inForce(directory: string, entry: string, base: string) {
  const script = `
    set -euo pipefail
    fail() { echo "FAIL: $*" >&2; exit 1; }
    note() { echo "   $*"; }
    source "$2"
    main_additions=("$3")
    main_additions_in_force "$1" "$4"
    echo "live \${#main_additions_live[@]}"
  `;
  return runBounded({
    argv: ["bash", "-c", script, "main-additions", directory, library, entry, base],
    bound: "short",
    cwd: directory,
    from: import.meta.url,
  });
}

test(
  "an entry whose commit this build holds and the base does not is in force",
  async () => {
    const { directory, root, change } = repository();
    const run = await inForce(directory, holdsEntry(change), root);
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout).toContain(`main addition in force: ${change} (main adds a member)`);
    expect(run.stdout).toContain("live 1");
  },
  rowLimit("short")
);

test(
  "an entry whose commit the base holds too is not in force, and admits nothing",
  async () => {
    const { directory, change } = repository();
    const run = await inForce(directory, holdsEntry(change), change);
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout).toContain(`main addition not in force: the base ${change} holds ${change}`);
    expect(run.stdout).toContain("live 0");
  },
  rowLimit("short")
);

test(
  "an entry whose commit this build does not hold fails the run, naming it",
  async () => {
    const { directory, root, beside } = repository();
    const run = await inForce(directory, holdsEntry(beside), root);
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(`FAIL: main_additions names main's ${beside}`);
    expect(run.stderr).toContain("does not hold it");
    expect(run.stdout).not.toContain("live");
  },
  rowLimit("short")
);

test(
  "an entry not written as COMMIT, ROUTE, PATH and VALUE fails the run",
  async () => {
    const { directory, root, change } = repository();
    for (const entry of [
      holdsEntry(change.slice(0, 10)),
      `${change}\t/api/v1/issues/{key}\t.design_gates[].holds\tnull`,
      `${change}\tGET /api/v1/issues/{key}\tdesign_gates[].holds\tnull`,
      `${change}\tGET /api/v1/issues/{key}\t.design_gates[]\tnull`,
      `${change}\tGET /api/v1/issues/{key}\t.design_gates[].holds\tnothing`,
    ]) {
      const run = await inForce(directory, entry, root);
      expect(run.code).toBe(1);
      expect(run.stderr).toContain(
        "FAIL: main_additions entry 1 is not COMMIT, METHOD PATTERN, PATH and VALUE"
      );
    }
  },
  rowLimit("short", "short", "short", "short", "short")
);

test(
  "an empty list puts nothing in force, admits no answer and fails nothing",
  async () => {
    const { directory, root } = repository();
    const work = scratch();
    mkdirSync(join(work, "answers"));
    writeFileSync(join(work, "route-index-head"), `${routeIndex.join("\n")}\n`);
    writeFileSync(
      join(work, "headers"),
      ["HTTP/1.1 200 OK", "Content-Type: application/json", "", ""].join("\r\n")
    );
    const script = `
      set -euo pipefail
      fail() { echo "FAIL: $*" >&2; exit 1; }
      note() { echo "   $*"; }
      work=$3
      run_epoch=$(date +%s)
      source "$2"
      : >"$work/ids-main.tsv"
      readonly main_additions=()
      main_additions_in_force "$1" "$4"
      status=200
      printf '%s' "$5" >"$work/body"
      keep_answer base main
      printf '%s' "$6" >"$work/body"
      keep_answer head main
      same_answers "the row" base head '' '' "GET /api/v1/issues/PUB-1"
      main_additions_report
      main_additions_used
      echo "live \${#main_additions_live[@]} differed $differed"
    `;
    const run = await runBounded({
      argv: [
        "bash",
        "-c",
        script,
        "main-additions",
        directory,
        library,
        work,
        root,
        issue(gate),
        issue(gateHolding("null")),
      ],
      bound: "short",
      cwd: directory,
      from: import.meta.url,
    });
    expect(run.stderr).toBe("");
    expect(run.code).toBe(0);
    expect(run.stdout).not.toContain("main addition");
    expect(run.stdout).toContain("live 0 differed 1");
  },
  rowLimit("short")
);
