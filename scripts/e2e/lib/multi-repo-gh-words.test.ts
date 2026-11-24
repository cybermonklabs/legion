import { afterEach, describe, expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { expectNoMarker, MARKER } from "./marker-runs";
import {
  closeServers,
  daemonStub,
  dispatchStub,
  fakeBin,
  fullInputs,
  isolatedPath,
  makeStateBody,
  newFakeDir,
  spawnScript,
  useScratchDirectory,
} from "./multi-repo-acceptance-harness";

// scripts/e2e/multi-repo-acceptance.sh prints no word of gh's: whatever answers its gh calls in
// GitHub's place holds gh's token and writes gh's stderr and its answer. Each row runs one
// observation of the script whole, against the harness's stubs, with a stand-in gh first on PATH
// that puts MARKER in its stderr or its answer, and holds all the observation printed (stdout and
// stderr) to no run of the marker (expectNoMarker). Its twin reads the file the observation names
// and finds the marker there, so the row could see it; its control is the stand-in's own log of
// the calls it answered. second-owner's rows are in multi-repo-second-owner.test.ts, beside the
// role's fakes they need.
useScratchDirectory();

afterEach(() => {
  closeServers();
});

const sha = "a".repeat(40);
const parent = "b".repeat(40);
const said = `gh: ${MARKER} (HTTP 502)`;

// A stand-in gh: each case is a `case "$*"` arm, `"<argv>") <shell>`; any other call is refused.
// Every call it is given is logged to CALLS first.
const standIn = (calls: string, arms: string[]) => `printf '%s\\n' "$*" >>'${calls}'
case "$*" in
${arms.map((arm) => `  ${arm} ;;`).join("\n")}
  *) echo "gh: unexpected call: $*" >&2; exit 1 ;;
esac`;
const fails = (argv: string) => `"${argv}") echo '${said}' >&2; exit 1`;
const answers = (argv: string, answer: string) => `"${argv}") printf '%s\\n' '${answer}'`;

// observe runs the observation ARGS against a daemon whose state is STATE, with the stand-in gh
// answering ARMS. SETUP writes into the evidence directory before the run.
async function observe(
  args: string[],
  arms: string[],
  options: { state?: string; setup?: (evidence: string) => void } = {}
) {
  const daemon = daemonStub(options.state);
  const dispatch = dispatchStub({});
  const env = fullInputs(`http://127.0.0.1:${daemon.port}`, `http://127.0.0.1:${dispatch.port}`);
  const { bin, evidence, work } = newFakeDir();
  const calls = join(work, "gh-calls");
  fakeBin(bin, "gh", standIn(calls, arms));
  options.setup?.(evidence);
  const result = await spawnScript(args, isolatedPath(bin), evidence, env);
  return {
    ...result,
    evidence,
    printed: result.stdout + result.stderr,
    calls: () => (existsSync(calls) ? readFileSync(calls, "utf8") : ""),
    // again runs a second observation against the same daemon, evidence directory and call log,
    // the stand-in answering NEXT_ARMS.
    again: (next: string[], nextArms: string[]) => {
      fakeBin(bin, "gh", standIn(calls, nextArms));
      return spawnScript(next, isolatedPath(bin), evidence, env);
    },
  };
}

// expectKept holds what an observation printed to no run of the marker, and the file it names to
// holding the marker (the twin).
function expectKept(printed: string, kept: string) {
  expectNoMarker(printed);
  expect(printed).toContain(kept);
  expect(readFileSync(kept, "utf8")).toContain(MARKER);
}

const defaultT = "api repos/acme/widgets --jq .default_branch";
const defaultC = "api repos/acme/gadgets --jq .default_branch";
// recorded writes the record-head.txt a passing record-head leaves, naming PARENT as T's head.
const recorded = (evidence: string) =>
  writeFileSync(
    join(evidence, "record-head.txt"),
    `repo: acme/widgets\ndefault branch: main\nhead: ${parent}\n`
  );

describe("record-head prints no word of gh's", () => {
  test("gh's own words on a failed default-branch read are kept in a file it names", async () => {
    const run = await observe(["record-head", "acme/widgets"], [fails(defaultT)]);
    expect(run.status).toBe(1);
    expectKept(run.printed, join(run.evidence, "record-head-default-branch.raw.gh-stderr"));
    expect(run.calls()).toBe(`${defaultT}\n`);
  }, 30_000);

  test("gh's own words on a failed head read are kept in a file it names", async () => {
    const branch = "api repos/acme/widgets/branches/main --jq .commit.sha";
    const run = await observe(
      ["record-head", "acme/widgets"],
      [answers(defaultT, "main"), fails(branch)]
    );
    expect(run.status).toBe(1);
    expectKept(run.printed, join(run.evidence, "record-head-branch.raw.gh-stderr"));
    expect(run.calls()).toBe(`${defaultT}\n${branch}\n`);
  }, 30_000);

  // The head read's refusal named the request it made, the default branch GitHub answered in it.
  test("a failed head read names no default branch GitHub answered", async () => {
    const branch = `api repos/acme/widgets/branches/${MARKER} --jq .commit.sha`;
    const run = await observe(
      ["record-head", "acme/widgets"],
      [answers(defaultT, MARKER), `"${branch}") echo 'gh: HTTP 502' >&2; exit 1`]
    );
    expect(run.status).toBe(1);
    expectNoMarker(run.printed);
    expect(run.printed).toContain("gh api repos/acme/widgets/branches/<its default branch>");
    expect(readFileSync(join(run.evidence, "record-head-default-branch.raw"), "utf8")).toContain(
      MARKER
    );
    expect(run.calls()).toBe(`${defaultT}\n${branch}\n`);
  }, 30_000);
});

describe("branch-base prints no word of gh's", () => {
  const compare = (branch: string) =>
    `api repos/acme/widgets/compare/${branch}...legion/LEGION-2 --jq .commits[0].sha`;
  const commit = `api repos/acme/widgets/commits/${sha} --jq .parents[0].sha`;

  // A default branch and a first commit that are the marker: neither is a value branch-base holds
  // to a closed grammar it may print, so its refusal names the files that keep them.
  test("a default branch and a first commit GitHub answered are named by their files", async () => {
    const run = await observe(
      ["branch-base", "LEGION-2"],
      [answers(defaultT, MARKER), answers(compare(MARKER), MARKER)],
      { setup: recorded }
    );
    expect(run.status).toBe(1);
    expectKept(run.printed, join(run.evidence, "branch-base-compare.raw"));
    expect(readFileSync(join(run.evidence, "branch-base-default-branch.raw"), "utf8")).toContain(
      MARKER
    );
    expect(run.calls()).toBe(`${defaultT}\n${compare(MARKER)}\n`);
  }, 30_000);

  // The compare's refusal named the request it made, the default branch GitHub answered in it.
  test("a failed compare names no default branch GitHub answered", async () => {
    const run = await observe(
      ["branch-base", "LEGION-2"],
      [answers(defaultT, MARKER), `"${compare(MARKER)}") echo 'gh: HTTP 502' >&2; exit 1`],
      { setup: recorded }
    );
    expect(run.status).toBe(1);
    expectNoMarker(run.printed);
    expect(run.printed).toContain(
      "gh api repos/acme/widgets/compare/<its default branch>...legion/LEGION-2"
    );
    expect(readFileSync(join(run.evidence, "branch-base-default-branch.raw"), "utf8")).toContain(
      MARKER
    );
    expect(run.calls()).toBe(`${defaultT}\n${compare(MARKER)}\n`);
  }, 30_000);

  test("gh's own words on a failed compare are kept in a file it names", async () => {
    const run = await observe(
      ["branch-base", "LEGION-2"],
      [answers(defaultT, "main"), fails(compare("main"))],
      { setup: recorded }
    );
    expect(run.status).toBe(1);
    expectKept(run.printed, join(run.evidence, "branch-base-compare.raw.gh-stderr"));
    expect(run.calls()).toBe(`${defaultT}\n${compare("main")}\n`);
  }, 30_000);

  test("gh's own words on a failed commit read are kept in a file it names", async () => {
    const run = await observe(
      ["branch-base", "LEGION-2"],
      [answers(defaultT, "main"), answers(compare("main"), sha), fails(commit)],
      { setup: recorded }
    );
    expect(run.status).toBe(1);
    expectKept(run.printed, join(run.evidence, "branch-base-commit.raw.gh-stderr"));
    expect(run.calls()).toBe(`${defaultT}\n${compare("main")}\n${commit}\n`);
  }, 30_000);

  // Control: a first commit and its parent that are full commit ids still pass.
  test("a first commit whose parent is the recorded head is a PASS", async () => {
    const run = await observe(
      ["branch-base", "LEGION-2"],
      [answers(defaultT, "main"), answers(compare("main"), sha), answers(commit, parent)],
      { setup: recorded }
    );
    expect(run.stdout).toContain("CHECK branch-base: PASS");
    expect(run.status).toBe(0);
  }, 30_000);
});

describe("pin prints no word of gh's", () => {
  const contents = (branch: string) =>
    `api repos/acme/gadgets/contents/toolchain-pin.txt?ref=${branch} --jq .content`;

  test("the pin file GitHub answered is kept in pin.txt, never printed", async () => {
    const pinFile = Buffer.from(`pin: ${sha}\n# ${MARKER}\n`).toString("base64");
    const run = await observe(
      ["pin", "acme/gadgets", sha],
      [answers(defaultC, "main"), answers(contents("main"), pinFile)]
    );
    expect(run.stdout).toContain("CHECK pin: PASS");
    expect(run.status).toBe(0);
    expectKept(run.printed, join(run.evidence, "pin.txt"));
    expect(run.calls()).toBe(`${defaultC}\n${contents("main")}\n`);
  }, 30_000);

  test("an empty pin file on a default branch GitHub answered names neither", async () => {
    const run = await observe(
      ["pin", "acme/gadgets", sha],
      [answers(defaultC, MARKER), answers(contents(MARKER), "")]
    );
    expect(run.status).toBe(1);
    expectNoMarker(run.printed);
    expect(run.printed).toContain("toolchain-pin.txt on acme/gadgets's default branch is empty");
    expect(readFileSync(join(run.evidence, "pin-default-branch.raw"), "utf8")).toContain(MARKER);
    expect(run.calls()).toBe(`${defaultC}\n${contents(MARKER)}\n`);
  }, 30_000);

  test("gh's own words on a failed content read are kept in a file it names", async () => {
    const run = await observe(
      ["pin", "acme/gadgets", sha],
      [answers(defaultC, "main"), fails(contents("main"))]
    );
    expect(run.status).toBe(1);
    expectKept(run.printed, join(run.evidence, "pin.txt.b64.gh-stderr"));
    expect(run.calls()).toBe(`${defaultC}\n${contents("main")}\n`);
  }, 30_000);
});

describe("negative pr-in-x prints no word of gh's", () => {
  const pulls = "api repos/other/outside/pulls?state=all&per_page=100";
  const highest = `${pulls} --jq [.[].number] | max // 0`;
  const state = makeStateBody({
    issues: {
      "LEGION-2": { key: "LEGION-2", generation: 1, phase: "implementing", repo: "acme/widgets" },
    },
  });

  test("gh's own words on a failed read of X's highest number are kept in a file it names", async () => {
    const run = await observe(["negative", "pr-in-x", "before", "LEGION-2"], [fails(highest)], {
      state,
    });
    expect(run.status).toBe(1);
    expectKept(
      run.printed,
      join(run.evidence, "negative-pr-in-x-before-LEGION-2-highest-pr.raw.gh-stderr")
    );
    expect(run.calls()).toBe(`${highest}\n`);
  }, 30_000);

  // after's listing is read twice over: gh's own words on a failed read, and jq's on a listing it
  // cannot read, which quote the listing ("Cannot iterate over string (...)"). The run ends as it
  // did before either was kept: raw_fetch's fail (1) for the read, jq's own status (5, a runtime
  // error) for the count.
  for (const [name, arm, kept, status] of [
    ["gh's own words on a failed listing", fails(pulls), ".raw.gh-stderr", 1],
    [
      "jq's words on a listing that is no list",
      answers(pulls, JSON.stringify(MARKER)),
      ".raw.jq-stderr",
      5,
    ],
  ] as const) {
    test(`${name} are kept in a file it names`, async () => {
      const before = await observe(
        ["negative", "pr-in-x", "before", "LEGION-2"],
        [answers(highest, "4")],
        { state }
      );
      expect(before.status).toBe(0);
      const runId = before.stdout.match(/^RUN_ID: (\S+)$/m)?.[1] ?? "";
      expect(runId).not.toBe("");
      const after = await before.again(["negative", "pr-in-x", "after", runId, "LEGION-2"], [arm]);
      expect(after.status).toBe(status);
      expectKept(
        after.stdout + after.stderr,
        join(before.evidence, `negative-pr-in-x-after-LEGION-2-${runId}-pulls${kept}`)
      );
      expect(before.calls()).toBe(`${highest}\n${pulls}\n`);
    }, 30_000);
  }
});
