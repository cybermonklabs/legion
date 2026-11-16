import { describe, expect, test } from "bun:test";
import { bearerCredential } from "../../../packages/envoy-client/src/bearer.ts";
import {
  type DispatchWrites,
  type ExpectedMove,
  judgeControl,
  judgeMoved,
  liveDispatch,
  parseArguments,
  runCheck,
} from "../moved-approval-production-check.ts";
import { UsageError } from "../production-check-runtime.ts";

const ASK = "6f1c0b4a-7d2e-4a19-9f55-2c3d8e4b17a0";
const KEY = "ACME-12";

/** The move the check looks for: the ask its request opened, the issue it made, and the
 *  Verification document it wrote, asked about at version 1 and standing at version 2. */
const EXPECTED: ExpectedMove = {
  askID: ASK,
  issueKey: KEY,
  name: "verification.md",
  requestedVersion: 1,
  version: 2,
};

function document(name: string, requested: number, version: number) {
  return {
    artifact_id: "f4d2b0c6-9a31-4f0e-8f2b-1d6c5a7e9043",
    name,
    requested_version: requested,
    version,
  };
}

/** The report as the server sends it: the spec the request was opened on, untouched, and the
 *  Verification document the write stored a new version of. */
function report(overrides: object = {}) {
  return {
    ask_id: ASK,
    issue: KEY,
    documents: [document("spec.md", 1, 1), document("verification.md", 1, 2)],
    ...overrides,
  };
}

/** `POST /api/v1/artifacts/{id}/edits` as it answers, with the report on its advice or without. */
function answer(moved?: object) {
  return {
    applied: 1,
    changed: true,
    lost_ops: null,
    token: "token",
    unchanged_ops: null,
    version: { number: 2 },
    ...(moved === undefined
      ? {}
      : { advice: { decision_blocks: 0, moved_approval_request: moved } }),
  };
}

describe("the report of a write that moved the request", () => {
  test("a report naming the moved document passes and prints both versions of each", () => {
    expect(judgeMoved(answer(report()), EXPECTED)).toEqual({
      verdict: "pass",
      line: `moved ask=${ASK} issue=${KEY} documents=spec.md:1/1,verification.md:1/2 pass`,
    });
  });

  test("a report whose documents all stand at the version asked about fails", () => {
    const unmoved = report({
      documents: [document("spec.md", 1, 1), document("verification.md", 1, 1)],
    });
    expect(judgeMoved(answer(unmoved), EXPECTED)).toEqual({
      verdict: "fail",
      line: `moved ask=${ASK} issue=${KEY} documents=spec.md:1/1,verification.md:1/1 fail`,
    });
  });

  test("an answer with no report at all fails and names no document", () => {
    expect(judgeMoved(answer(), EXPECTED)).toEqual({
      verdict: "fail",
      line: "moved report=none fail",
    });
  });

  test("another request's ask and another issue are printed as other", () => {
    const foreign = report({ ask_id: "11111111-2222-3333-4444-555555555555", issue: "PRIV-9" });
    expect(judgeMoved(answer(foreign), EXPECTED)).toEqual({
      verdict: "fail",
      line: "moved ask=other issue=other documents=spec.md:1/1,verification.md:1/2 fail",
    });
  });

  test("a document the check did not upload is printed as other, never by name", () => {
    const foreign = report({ documents: [document("private-plan.md", 3, 4)] });
    const judgement = judgeMoved(answer(foreign), EXPECTED);
    expect(judgement).toEqual({
      verdict: "fail",
      line: `moved ask=${ASK} issue=${KEY} documents=other:3/4 fail`,
    });
    expect(judgement.line).not.toContain("private-plan.md");
  });

  test("a report that holds no document at all prints none", () => {
    expect(judgeMoved(answer(report({ documents: null })), EXPECTED)).toEqual({
      verdict: "fail",
      line: `moved ask=${ASK} issue=${KEY} documents=none fail`,
    });
  });

  test("a report that is not the shape the server sends is malformed", () => {
    expect(() => judgeMoved(answer({ issue: KEY, documents: [] }), EXPECTED)).toThrow();
  });
});

describe("the negative control", () => {
  test("an answer with no report passes", () => {
    expect(judgeControl(answer(), "notes.md")).toEqual({
      verdict: "pass",
      line: "control document=notes.md report=none pass",
    });
  });

  test("an answer carrying a report fails", () => {
    expect(judgeControl(answer(report()), "notes.md")).toEqual({
      verdict: "fail",
      line: "control document=notes.md report=present fail",
    });
  });
});

describe("parseArguments", () => {
  test("a project key is read", () => {
    expect(parseArguments(["--project", "ACME"])).toEqual({ project: "ACME" });
  });

  test("no --project is refused", () => {
    expect(() => parseArguments([])).toThrow(UsageError);
  });

  test("a value that is not a project key is refused without repeating it", () => {
    let caught: unknown;
    try {
      parseArguments(["--project", "acme-private-name"]);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(UsageError);
    expect((caught as UsageError).message).not.toContain("acme-private-name");
  });

  test("an unknown option is refused without repeating it", () => {
    let caught: unknown;
    try {
      parseArguments(["--project", "ACME", "--secret-host-name"]);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(UsageError);
    expect((caught as UsageError).message).not.toContain("secret-host-name");
  });

  test("a stray argument is refused", () => {
    expect(() => parseArguments(["--project", "ACME", "ACME-12"])).toThrow(UsageError);
  });
});

const SPEC_ID = "2b7f1d90-4c3a-4e58-9b21-7d0e6a5c4f83";
const VERIFICATION_ID = "c51a7e24-8f60-4b19-83d7-4e2a9c0b6d15";
const NOTES_ID = "9d3c6b08-2a74-4f51-b6e9-0c8f5a1d7243";

/** A Dispatch that refuses the closing reason and answers the close, writing down every request
 *  it was sent in the order it arrived. */
function closingServer(sent: string[]) {
  return Bun.serve({
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      sent.push(`${request.method} ${pathname}`);
      return pathname.endsWith("/messages")
        ? Response.json({ error: { code: "REFUSED", message: "no" } }, { status: 422 })
        : Response.json({ status: "done" });
    },
  });
}

/** Every write of a run whose steps all pass, with the close the test hands it: the issue is
 *  made, its three documents are uploaded, the request is opened on the spec, the write of
 *  `verification.md` carries the report and the write of `notes.md` carries none. */
function writes(close: DispatchWrites["close"]): DispatchWrites {
  return {
    healthz: async () => ({ ok: true, commit: "0123abcd", schema_version: 88 }),
    createIssue: async () => ({ key: KEY, primary_artifact_id: SPEC_ID, created: true }),
    setStatus: async () => ({ status: "in_progress" }),
    upload: async (_key, name) => ({
      artifact: { id: name === "verification.md" ? VERIFICATION_ID : NOTES_ID },
      version: { number: 1 },
    }),
    requestApproval: async () => ({ ask: { id: ASK }, version: 1 }),
    edit: async (artifactID) => (artifactID === VERIFICATION_ID ? answer(report()) : answer()),
    close,
    docEditCommand: async () => ({ code: 0, line: true }),
  };
}

describe("the close of the scratch issue", () => {
  test("the status write is sent although the closing reason was refused", async () => {
    const sent: string[] = [];
    const server = closingServer(sent);
    try {
      const live = liveDispatch({
        url: `http://127.0.0.1:${server.port}`,
        credential: bearerCredential("t"),
      });
      const out: string[] = [];
      const code = await runCheck(
        parseArguments(["--project", "ACME"]),
        writes(live.close),
        (line) => out.push(line)
      );
      expect(sent).toEqual([`POST /api/v1/issues/${KEY}/messages`, `PATCH /api/v1/issues/${KEY}`]);
      expect(out).toContain(`close issue=${KEY} status=done pass`);
      expect(out.at(-1)).toBe("all 3 assertions passed");
      expect(code).toBe(0);
    } finally {
      server.stop(true);
    }
  });
});
