import { describe, expect, test } from "bun:test";
import type { CheckResult, IssueChain, IssueChainResult } from "@legion/contracts";
import { fakeDispatchFetch } from "../../test/fake-dispatch";
import { bearerCredential } from "../bearer";
import { executeDispatchTool } from "../dispatch-execute";

// `dispatch chain --issue <KEY>`: the issue's chain as Dispatch's `GET /api/v1/issues/{key}/chain`
// answers it, each requirement with the decisions that shaped it and the checks that serve it,
// then each check once with its standing and its results, each plan task, and the gaps. Each case
// drives `executeDispatchTool` over the shared fake Dispatch (`test/fake-dispatch.ts`) as a fetch
// and reads the text and the details a host is handed.

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};

const COMMIT = "1111111111111111111111111111111111111111";
const AT = "2026-11-21T09:00:00Z";
const ASK = "01234567-0000-4000-8000-0000000000a1";
const PASSED = "01234567-0000-4000-8000-0000000000c1";
const FAILED = "01234567-0000-4000-8000-0000000000c2";
const RETRACTED = "01234567-0000-4000-8000-0000000000c3";

/** A node of DSP-1's tree by its block id. */
function node(block: string): string {
  return `DSP-1/tree#${block}`;
}

/** One result of a check, as the chain carries it: the results read's record of it, with the
 *  walk back from it to the requirements its check serves and the decisions behind them. */
function result(
  id: string,
  check: string,
  recorded: Partial<CheckResult> & Pick<CheckResult, "verdict">,
  walk: Pick<IssueChainResult, "proves" | "requirements"> & Partial<IssueChainResult>
): IssueChainResult {
  return {
    result: {
      id,
      issue_key: "DSP-1",
      check_issue_key: "DSP-1",
      check_artifact_id: "01234567-0000-4000-8000-00000000000a",
      check_block_id: check,
      check_version: 1,
      check_sha256: "2222222222222222222222222222222222222222222222222222222222222222",
      role: "implementer",
      surface: "cli",
      run: "bun test",
      observed: "",
      commit_sha: COMMIT,
      negative_control: "",
      failed: 0,
      skipped: 0,
      excluded: 0,
      screenshots: [],
      session: "session-1",
      actor: { kind: "session", id: "session-1" },
      created_at: AT,
      commit_stands: { answer: "at_basis", basis: { kind: "named", commit: COMMIT } },
      check_words_moved: false,
      ...recorded,
    },
    check: node(check),
    decisions: [],
    ...walk,
  };
}

/** DSP-1's chain: r1 and r2 under two decisions, r1 and r2 served by c1 whose pass stands, r2 by
 *  c2 whose newest result failed and whose pass was retracted after its words moved, r3 shaped
 *  and served by no check, and a plan task that serves nothing. */
function chainAnswer(at: string | null): IssueChain {
  const issueParent = { kind: "issue", id: "DSP-1", supertag: "issue", first_line: "Native" };
  return {
    issue: "DSP-1",
    at,
    requirements: [
      {
        id: node("r1"),
        block_id: "r1",
        ref: "dispatch://DSP-1/spec#b-r1",
        text: "The workspace opens\nnatively.",
        state: "live",
        parent: issueParent,
        decisions: [node("d1"), ASK],
        checks: [node("c1")],
        plan_tasks: [],
        proved: true,
      },
      {
        id: node("r2"),
        block_id: "r2",
        ref: "dispatch://DSP-1/spec#b-r2",
        text: "An ask keeps its urgency.",
        state: "live",
        parent: issueParent,
        decisions: [],
        checks: [node("c1"), node("c2")],
        plan_tasks: [],
        proved: true,
      },
      {
        id: node("r3"),
        block_id: "r3",
        ref: "dispatch://DSP-1/spec#b-r3",
        text: "A plan node says its size.",
        state: "live",
        parent: issueParent,
        decisions: [node("d1")],
        checks: [],
        plan_tasks: [],
        proved: false,
      },
    ],
    decisions: [
      {
        kind: "block",
        id: node("d1"),
        block_id: "d1",
        text: "The workspace is native.",
        shaped: [node("r1"), node("r3")],
      },
      {
        kind: "ask",
        id: ASK,
        text: "Native or remote?",
        answer: "Native, so it opens offline.",
        shaped: [node("r1")],
      },
    ],
    checks: [
      {
        id: node("c1"),
        block_id: "c1",
        ref: "dispatch://DSP-1/artifact/verification-md#b-c1",
        issue: "DSP-1",
        text: "The workspace opens.",
        when: "before_merge",
        state: "live",
        parent: {
          kind: "block",
          id: node("r1"),
          supertag: "requirement",
          first_line: "The workspace",
        },
        serves: [node("r1"), node("r2")],
        version: 1,
        sha256: "3333333333333333333333333333333333333333333333333333333333333333",
        compared_with: [{ kind: "named", commit: COMMIT }],
        pass_stands: { answer: "yes", roles: ["implementer"] },
        results: [PASSED],
        proved: true,
        unproven: [],
      },
      {
        id: node("c2"),
        block_id: "c2",
        ref: "dispatch://DSP-1/artifact/verification-md#b-c2",
        issue: "DSP-1",
        text: "The urgency stays.",
        when: "before_merge",
        state: "live",
        parent: { kind: "block", id: node("r2"), supertag: "requirement", first_line: "An ask" },
        serves: [node("r2")],
        version: 2,
        sha256: "4444444444444444444444444444444444444444444444444444444444444444",
        compared_with: [{ kind: "named", commit: COMMIT }],
        pass_stands: { answer: "no" },
        results: [FAILED, RETRACTED],
        proved: false,
        unproven: [
          { reason: "failed", result: FAILED },
          { reason: "retracted", result: RETRACTED },
          { reason: "check_words_moved", result: RETRACTED },
        ],
      },
    ],
    plan_tasks: [
      {
        id: node("t1"),
        block_id: "t1",
        ref: "dispatch://DSP-1/spec#b-t1",
        issue: "DSP-1",
        text: "Tidy the list.",
        state: "live",
        parent: issueParent,
        serves: [],
      },
    ],
    results: [
      result(
        PASSED,
        "c1",
        { verdict: "pass", observed: "bun test: 12 pass,\n0 fail." },
        { proves: true, requirements: [node("r1"), node("r2")], decisions: [node("d1"), ASK] }
      ),
      result(
        FAILED,
        "c2",
        { verdict: "fail", role: "tester", failed: 1 },
        { proves: false, requirements: [node("r2")] }
      ),
      result(
        RETRACTED,
        "c2",
        {
          verdict: "pass",
          check_words_moved: true,
          retracted: {
            reason: "The run named the wrong commit.",
            actor: { kind: "user", id: "alice" },
            created_at: AT,
          },
        },
        { proves: false, requirements: [node("r2")] }
      ),
    ],
    gaps: [
      { kind: "no_decision", node: node("r2"), line: "r2 has no decision" },
      { kind: "no_check", node: node("r3"), line: "r3 has no check" },
      { kind: "serves_nothing", node: node("t1"), line: "t1 serves nothing" },
    ],
  };
}

/** `dispatch_chain` with `args` over a fake Dispatch whose chain route of DSP-1 answers `answer`:
 *  the result, and each request the fake saw as `<method> <pathname><search>`. */
async function readChain(args: Record<string, unknown>, answer: IssueChain) {
  const dispatch = fakeDispatchFetch({ routes: { "/api/v1/issues/DSP-1/chain": answer } });
  const result = await executeDispatchTool({
    tool: "dispatch_chain",
    args,
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-1",
    config,
    env: {},
    exec: async (file, argv) => {
      throw new Error(`unexpected command: ${file} ${argv.join(" ")}`);
    },
    fetchImpl: dispatch.fetchImpl,
  });
  return {
    result,
    requests: dispatch.calls.map((call) => `${call.method} ${call.url.pathname}${call.url.search}`),
  };
}

describe("dispatch chain", () => {
  test("prints each requirement with its decisions and its checks, each check once with its standing, and the gaps", async () => {
    const { result, requests } = await readChain(
      { issue: "DSP-1", at: COMMIT },
      chainAnswer(COMMIT)
    );

    expect(requests).toEqual([`GET /api/v1/issues/DSP-1/chain?at=${COMMIT}`]);
    expect(result.text).toBe(
      [
        `DSP-1: 3 requirements, 2 decisions, 2 checks, 3 results, judged at ${COMMIT}.`,
        "Requirement r1, proved: The workspace opens natively.",
        "  Decision d1: The workspace is native.",
        `  Decision ask ${ASK}: Native or remote? The answer: Native, so it opens offline.`,
        "  Check c1, proved: The workspace opens.",
        "Requirement r2, proved: An ask keeps its urgency.",
        "  No decision shaped it.",
        "  Check c1, proved: The workspace opens.",
        "  Check c2, unproven: The urgency stays.",
        "Requirement r3, unproven: A plan node says its size.",
        "  Decision d1: The workspace is native.",
        "  No check serves it.",
        "Check c1 of DSP-1, under r1, serving r1 r2: The workspace opens.",
        "  A pass stands: yes, from implementer.",
        `  Result ${PASSED}: pass by implementer at ${COMMIT}. That commit is the commit you named. The check's words did not move after it. It proves the check.`,
        "    Reading: bun test: 12 pass, 0 fail.",
        "Check c2 of DSP-1, under r2, serving r2: The urgency stays.",
        "  A pass stands: no.",
        `  Unproven: result ${FAILED} failed; result ${RETRACTED} is retracted; result ${RETRACTED} ran against words the check no longer holds.`,
        `  Result ${FAILED}: fail by tester at ${COMMIT}. That commit is the commit you named. The check's words did not move after it. It proves nothing.`,
        "    The role recorded no reading.",
        `  Result ${RETRACTED}: pass by implementer at ${COMMIT}. That commit is the commit you named. The check's words moved after it. user alice retracted it at ${AT}: The run named the wrong commit. It proves nothing.`,
        "    The role recorded no reading.",
        "Plan task t1 of DSP-1, under DSP-1, serving nothing: Tidy the list.",
        "Gaps:",
        "- r2 has no decision",
        "- r3 has no check",
        "- t1 serves nothing",
      ].join("\n")
    );
    expect(result.details).toEqual({
      issue: "DSP-1",
      at: COMMIT,
      requirements: 3,
      checks: 2,
      results: 3,
      gaps: ["r2 has no decision", "r3 has no check", "t1 serves nothing"],
    });
  });

  test("with no commit asks the route for none, and says each check is compared as dispatch results compares it", async () => {
    const answer = chainAnswer(null);
    const unproven: IssueChain = {
      ...answer,
      checks: (answer.checks ?? []).map((check) => ({
        ...check,
        compared_with: [],
        compared_nothing: "no_pull_request_or_branch",
        pass_stands: { answer: "no" },
        proved: false,
        unproven: [{ reason: "compared_nothing" }],
      })),
      gaps: [],
    };

    const { result, requests } = await readChain({ issue: "DSP-1" }, unproven);

    expect(requests).toEqual(["GET /api/v1/issues/DSP-1/chain"]);
    const lines = result.text.split("\n");
    expect(lines[0]).toBe(
      "DSP-1: 3 requirements, 2 decisions, 2 checks, 3 results, each check compared as dispatch results compares it."
    );
    expect(lines).toContain(
      "  Unproven: it is compared with nothing: the issue has no pull request and no branch to compare with."
    );
    expect(lines.at(-1)).toBe("Gaps: none.");
  });
});
