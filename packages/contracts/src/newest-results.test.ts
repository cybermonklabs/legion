import { describe, expect, test } from "bun:test";

import type { CheckBasis, CheckResult } from "./dispatch-api";
import { newestResultsText } from "./newest-results";

const head: CheckBasis = { kind: "pull_request", commit: "a".repeat(40), pull_request: 7 };
const named: CheckBasis = { kind: "named", commit: "b".repeat(40) };
const main: CheckBasis = { kind: "branch", branch: "main", commit: "c".repeat(40) };
const release: CheckBasis = { kind: "branch", branch: "release", commit: "d".repeat(40) };

/** A result as the API serves it: a pass by `role`, at a compared commit, recorded at 10:00. */
function result(role: string, id: string, fields: Partial<CheckResult> = {}): CheckResult {
  return {
    actor: { id: "session-1", kind: "session" },
    check_artifact_id: "artifact-9",
    check_block_id: "k1",
    check_issue_key: "DSP-42",
    check_sha256: "e".repeat(64),
    check_version: 4,
    check_words_moved: false,
    commit_sha: "a".repeat(40),
    commit_stands: { answer: "at_basis", basis: head },
    created_at: "2026-11-10T10:00:00Z",
    excluded: 0,
    failed: 0,
    id,
    issue_key: "DSP-42",
    negative_control: "",
    observed: "",
    role,
    run: "bash scripts/e2e/verification-results.sh",
    screenshots: null,
    session: "session-1",
    skipped: 0,
    surface: "cli",
    verdict: "pass",
    ...fields,
  };
}

const implementerPass = result("implementer", "2120a467-implementer");
const testerFail = result("tester", "9f5fd8c4-tester", {
  created_at: "2026-11-10T11:00:00Z",
  verdict: "fail",
});

describe("newestResultsText", () => {
  test("names each role's newest result oldest first, and says newer where it was recorded later", () => {
    expect(
      newestResultsText({ compared_with: [head], results: [testerFail, implementerPass] })
    ).toBe("Newest at that commit: implementer: pass (2120a467); tester: fail (9f5fd8c4, newer)");
  });

  test("an older result of a role behind its newest is not named", () => {
    const olderTester = result("tester", "0badc0de-tester", { created_at: "2026-11-10T09:00:00Z" });
    expect(
      newestResultsText({
        compared_with: [head],
        results: [testerFail, implementerPass, olderTester],
      })
    ).toBe("Newest at that commit: implementer: pass (2120a467); tester: fail (9f5fd8c4, newer)");
  });

  test("two results recorded at one moment are named with no newer", () => {
    const same = result("tester", "9f5fd8c4-tester", { verdict: "fail" });
    expect(newestResultsText({ compared_with: [head], results: [same, implementerPass] })).toBe(
      "Newest at that commit: implementer: pass (2120a467); tester: fail (9f5fd8c4)"
    );
  });

  test("a retracted result is left out, and the role's newest result that stands is named", () => {
    const retracted = result("tester", "77777777-tester", {
      created_at: "2026-11-10T12:00:00Z",
      retracted: {
        actor: { id: "alice", kind: "user" },
        created_at: "2026-11-10T12:30:00Z",
        reason: "It read a stale build.",
      },
    });
    expect(
      newestResultsText({
        compared_with: [head],
        results: [retracted, testerFail, implementerPass],
      })
    ).toBe("Newest at that commit: implementer: pass (2120a467); tester: fail (9f5fd8c4, newer)");
  });

  test("a result at no compared commit is left out", () => {
    const elsewhere = result("tester", "88888888-tester", {
      commit_sha: "f".repeat(40),
      commit_stands: { answer: "other_commit" },
      created_at: "2026-11-10T12:00:00Z",
    });
    const notKnown = result("reviewer", "99999999-reviewer", {
      commit_stands: { answer: "unknown" },
    });
    const unsaid = result("tester", "66666666-tester", { commit_stands: undefined });
    expect(
      newestResultsText({
        compared_with: [head],
        results: [elsewhere, notKnown, unsaid, implementerPass],
      })
    ).toBe("Newest at that commit: implementer: pass (2120a467)");
  });

  test("a result the check's words moved after says so", () => {
    const moved = result("implementer", "2120a467-implementer", { check_words_moved: true });
    expect(newestResultsText({ compared_with: [head], results: [testerFail, moved] })).toBe(
      "Newest at that commit: implementer: pass (2120a467, the check's words moved after it); tester: fail (9f5fd8c4, newer)"
    );
  });

  test.each([
    ["one head", [head], "at that commit"],
    ["one named commit", [named], "at that commit"],
    ["a head and a named commit", [head, named], "at those commits"],
    ["one branch", [main], "on that branch"],
    ["two branches", [main, release], "on those branches"],
    ["a head and a branch", [head, main], "at what the check compared with"],
  ])("a check compared with %s names the place %s", (_, bases, place) => {
    expect(newestResultsText({ compared_with: bases, results: [implementerPass] })).toBe(
      `Newest ${place}: implementer: pass (2120a467)`
    );
  });

  test("answers nothing where the check compared with nothing, or no result stands there", () => {
    expect(newestResultsText({ compared_with: [], results: [implementerPass] })).toBeUndefined();
    expect(newestResultsText({ compared_with: null, results: [implementerPass] })).toBeUndefined();
    expect(newestResultsText({ compared_with: [head], results: [] })).toBeUndefined();
    expect(newestResultsText({ compared_with: [head], results: null })).toBeUndefined();
    const elsewhere = result("tester", "88888888-tester", {
      commit_stands: { answer: "other_commit" },
    });
    expect(newestResultsText({ compared_with: [head], results: [elsewhere] })).toBeUndefined();
  });
});
