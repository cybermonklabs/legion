import { beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  holdsMarker,
  marker,
  markerLines,
  type Rule,
  writeRules,
  writeStandInGh,
} from "./gh-words";
import { scriptFunctions } from "./script-functions";

// parked-decision-devbox.sh's own gh calls, each taken from the script by name with the check
// helpers it prints through and lib/workflow.sh's gh_kept, and run under the script's own `set`
// line against a stand-in gh first on PATH (gh-words.ts) whose stderr or answer holds the marker
// not-a-secret-token. Whatever answers gh in GitHub's place holds gh's token, so no line the run
// prints may hold a run of eight of the marker's characters; the twin is the file under
// $evidence/gh that keeps the marker, which the run names, and the control the stand-in's log of
// the call.
const scriptPath = join(import.meta.dir, "..", "parked-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const workflow = scriptFunctions(join(import.meta.dir, "workflow.sh"));
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);
const dir = temporaryDirectory("parked-decision-gh-words-test");
const bin = join(dir, "bin");
beforeAll(() => writeStandInGh(bin));

const says = `gh: ${marker} (HTTP 502)\n`;
const tree = "LEGION-1";

let runs = 0;
function run(call: string, rules: Rule[], kept: string) {
  const runDir = join(dir, `run-${++runs}`);
  const ghDir = join(runDir, "gh-stand-in");
  mkdirSync(join(runDir, "evidence"), { recursive: true });
  writeRules(ghDir, rules);
  const script = `${set}
check=cleanup-is-complete evidence="$RUN/evidence" repo=sjawhar/legion-smoke proof_human='sjawhar-agent[bot]'
tree1=${tree} tree2= tree3= tree4= tree5=
${fn("note")}
${fn("fail")}
${workflow("gh_kept")}
${fn("remove_run_branches")}
${fn("proof_human_reads_repo")}
${fn("run_branches_gone")}
${call}
`;
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: { PATH: `${bin}:${process.env.PATH}`, HOME: runDir, RUN: runDir, GH_DIR: ghDir },
  });
  const keptPath = join(runDir, "evidence", "gh", kept);
  return {
    code: result.exitCode,
    output: result.stdout.toString() + result.stderr.toString(),
    calls: readFileSync(join(ghDir, "calls"), "utf8").split("\n").filter(Boolean),
    keptPath,
    kept: existsSync(keptPath) ? readFileSync(keptPath, "utf8") : "",
  };
}

const rows = [
  {
    name: "remove_run_branches: the number of the pull request gh listed",
    call: "remove_run_branches",
    rules: [
      { match: `* pr list --head legion/${tree} *`, out: `${marker}\n` },
      { match: "* pr close * --comment *" },
      { match: `* api -X DELETE repos/*/git/refs/heads/legion/${tree} *` },
    ],
    kept: `run-branch-pull-request-${tree}.answer`,
    piece: `pr close ${marker} --comment`,
  },
  {
    name: "proof_human_reads_repo: gh's stderr",
    call: "proof_human_reads_repo",
    rules: [{ match: "* repo view sjawhar/legion-smoke --json name *", code: 1, err: says }],
    kept: "repo-view.err",
    piece: "repo view sjawhar/legion-smoke --json name",
  },
  {
    name: "run_branches_gone: a branch read that is neither the branch nor GitHub's 404",
    call: "run_branches_gone",
    rules: [{ match: `* api repos/*/branches/legion/${tree} *`, code: 1, err: says }],
    kept: `run-branch-${tree}.log`,
    piece: `api repos/sjawhar/legion-smoke/branches/legion/${tree}`,
  },
];

describe("parked-decision-devbox.sh prints no word of gh's", () => {
  test.each(rows.map((row) => [row.name, row] as const))("%s", (_name, row) => {
    const r = run(row.call, row.rules, row.kept);
    // The row: no line the run printed holds a run of the marker.
    expect(markerLines(r.output)).toEqual([]);
    // The twin: the kept file holds the marker, and the run names it.
    expect(holdsMarker(r.kept)).toBe(true);
    expect(r.output).toContain(r.keptPath);
    // The control: the stand-in gh answered the call the row is about.
    expect(r.calls.some((c) => c.includes(row.piece))).toBe(true);
  });

  // The verdicts the script had stay: GitHub's 404 is a branch gone, an answer is a branch that
  // remains, and a pull request number of digits is still named by it.
  test("run_branches_gone passes on GitHub's 404 and fails on a branch that remains", () => {
    const branchRead = `* api repos/*/branches/legion/${tree} *`;
    const gone = run(
      "run_branches_gone",
      [{ match: branchRead, code: 1, err: "gh: Branch not found (HTTP 404)\n" }],
      "x"
    );
    expect(gone.code).toBe(0);
    expect(gone.output).toBe("");
    const left = run("run_branches_gone", [{ match: branchRead, out: "{}\n" }], "x");
    expect(left.code).toBe(1);
    expect(left.output).toBe(
      `FAIL cleanup-is-complete: sjawhar/legion-smoke still has the branch legion/${tree}\n`
    );
  });

  test("remove_run_branches names a pull request of digits by its number", () => {
    const r = run(
      "remove_run_branches",
      [
        { match: `* pr list --head legion/${tree} *`, out: "12\n" },
        { match: "* pr close 12 --comment *" },
        { match: `* api -X DELETE repos/*/git/refs/heads/legion/${tree} *` },
      ],
      "x"
    );
    expect(r.code).toBe(0);
    expect(r.output).toBe(
      `   closed the run's open pull request sjawhar/legion-smoke#12 (legion/${tree})\n   deleted the run's branch legion/${tree} from sjawhar/legion-smoke\n`
    );
  });
});
