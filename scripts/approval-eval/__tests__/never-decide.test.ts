import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseRow } from "../build-corpus";
import type { ClaudeAsk, ModelAnswer } from "../models";
import { type AskModel, type IssueFacts, NEVER_DECIDE_KINDS, neverDecide } from "../never-decide";

/**
 * The rule over a fake asking function: nothing here makes a call, reads a corpus or reads
 * Dispatch. What it holds the rule to is the four things the mechanism turns on - the kind and
 * the reason come back from the model's own answer, an answer in no readable shape is a fault
 * and never a decide, the root on the backlog is held from the issue's facts with no call made,
 * and the cache answers the second asking of one document while a changed definition does not.
 */

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "never-decide-"));
  made.push(directory);
  return directory;
}

/** One approval case, with only the fields the rule reads named. */
function caseRow(artifactId: string | null = "artifact-1"): CaseRow {
  return {
    caseId: "case-1",
    eventId: 1,
    askId: "ask-1",
    issueKey: "PROJ-1",
    project: "PROJ",
    kind: "approval",
    urgency: "med",
    multiple: false,
    question: "Approve spec.md (version 3)?",
    options: [
      { id: "b2", label: "Approve", description: null },
      { id: "b3", label: "Request changes", description: null },
    ],
    selected: ["Approve"],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: "2026-11-06T12:00:00Z",
    eventCreatedAt: "2026-11-06T12:00:00Z",
    approval: {
      documents: [{ artifactId, name: "spec.md", version: 3, requestedVersion: 3 }],
      artifactSlug: "spec",
      artifactPrimary: true,
    },
    verdict: "approve",
    confounds: {
      version: 3,
      answersThatDayBefore: 0,
      hourUtc: 12,
      hourPacific: 4,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: "approval",
    },
  };
}

/** The issue facts of a root carrying the `legion` label that waits for a person. */
function waitingRoot(overrides: Partial<IssueFacts> = {}): IssueFacts {
  return {
    parent: null,
    labels: ["legion"],
    status: "backlog",
    primaryArtifactId: "artifact-1",
    ...overrides,
  };
}

/** One answer of the endpoint, as the asking function hands one back. */
function modelAnswer(text: string, labels: readonly string[]): ModelAnswer {
  const chosen = labels.filter((label) =>
    text
      .split("\n")
      .map((line) => line.trim().toLowerCase())
      .includes(label.toLowerCase())
  );
  return {
    chosen,
    text,
    probabilities: null,
    confidence: null,
    inputTokens: 1_200,
    outputTokens: 40,
    latencyMs: 10,
    attempts: 1,
    stateCut: false,
    fault: chosen.length === 0 ? "unreadable" : null,
    faultDetail: chosen.length === 0 ? text : null,
  };
}

/** What one fake asking function was asked, and what it answered. */
interface Fake {
  ask: AskModel;
  asks: ClaudeAsk[];
}

/** An asking function that answers `text` to every call and keeps what it was asked. */
function fake(text: string): Fake {
  const asks: ClaudeAsk[] = [];
  return {
    asks,
    ask: (ask) => {
      asks.push(ask);
      return Promise.resolve(modelAnswer(text, ask.prompt.labels));
    },
  };
}

/** The prose a test applies, which stands in for the file the shipped skill carries. */
const DEFINITION = "The rule, in prose: hold a document that asks for one of the seven kinds.";

/** One document, as a change list puts its act. */
const DOCUMENT = "# A change\n\n## Change\n\n- Drop the reports_legacy database on the primary.\n";

describe("neverDecide", () => {
  test("holds under the kind the model named, with the model's own reason", async () => {
    const asked = fake(
      "destructive-act\nThe change list asks for the reports_legacy database to be dropped."
    );
    const answer = await neverDecide(
      { text: DOCUMENT, row: caseRow(), documentId: "artifact-1", documentVersion: 3 },
      { ask: asked.ask, definition: DEFINITION, model: "a-model" }
    );
    expect({ held: answer.held, kind: answer.kind, source: answer.source }).toEqual({
      held: true,
      kind: "destructive-act",
      source: "model",
    });
    expect(answer.reason).toBe(
      "The change list asks for the reports_legacy database to be dropped."
    );
    expect(answer.fault).toBeNull();
    expect(answer.answer).toContain("destructive-act");
    expect([answer.inputTokens, answer.outputTokens]).toEqual([1_200, 40]);
    expect(asked.asks).toHaveLength(1);
    expect(asked.asks[0]?.model).toBe("a-model");
    expect(asked.asks[0]?.prompt.system).toContain(DEFINITION);
    expect(asked.asks[0]?.prompt.user).toContain("Drop the reports_legacy database");
  });

  test("decides a document the model named none of the kinds for", async () => {
    const asked = fake("none\nThe document reports a deletion that a retention job already made.");
    const answer = await neverDecide(
      { text: DOCUMENT },
      { ask: asked.ask, definition: DEFINITION }
    );
    expect({ held: answer.held, kind: answer.kind, fault: answer.fault }).toEqual({
      held: false,
      kind: null,
      fault: null,
    });
    expect(answer.reason).toBe(
      "The document reports a deletion that a retention job already made."
    );
  });

  test("answers a fault, and never a decide, where the answer is in no readable shape", async () => {
    for (const text of [
      "I think this one is probably fine to approve.",
      "destructive-act",
      "destructive-act\ncredential\nIt asks for both.",
    ]) {
      const asked = fake(text);
      const answer = await neverDecide(
        { text: DOCUMENT },
        { ask: asked.ask, definition: DEFINITION }
      );
      expect({ text, held: answer.held, kind: answer.kind }).toEqual({
        text,
        held: false,
        kind: null,
      });
      expect(answer.fault).not.toBeNull();
      expect(answer.reason).toContain("the rule could not be applied");
    }
  });

  test("holds the primary document of a legion root that waits, with no call made", async () => {
    for (const [status, words] of [
      ["backlog", "on the backlog"],
      ["icebox", "in icebox"],
    ]) {
      const asked = fake("none\nNothing of the kinds is asked for.");
      const answer = await neverDecide(
        {
          text: "# A root\n\nIt says what the tree will do.\n",
          row: caseRow(),
          issue: waitingRoot({ status }),
        },
        { ask: asked.ask, definition: DEFINITION }
      );
      expect({ held: answer.held, kind: answer.kind, source: answer.source }).toEqual({
        held: true,
        kind: "waiting-legion-root",
        source: "issue-facts",
      });
      expect(answer.reason).toBe(
        `the issue is a root carrying the legion label, ${words}, and this is its primary document`
      );
      expect(asked.asks).toHaveLength(0);
      expect(answer.answer).toBeNull();
    }
  });

  test("asks the model for a waiting issue that is not a root, not primary, or not waiting", async () => {
    const text = "# A root\n\nIt says what the tree will do.\n";
    const issues = [
      waitingRoot({ parent: "PROJ-9" }),
      waitingRoot({ labels: [] }),
      waitingRoot({ status: "in_progress" }),
      waitingRoot({ primaryArtifactId: "artifact-2" }),
    ];
    for (const issue of issues) {
      const asked = fake("none\nIt asks for nothing of the kinds.");
      const answer = await neverDecide(
        { text, row: caseRow(), issue },
        { ask: asked.ask, definition: DEFINITION }
      );
      expect(answer.held).toBe(false);
      expect(asked.asks).toHaveLength(1);
    }
  });

  test("reads the second asking of one document from the cache, and asks once", async () => {
    const directory = await scratch();
    const asked = fake("credential\nThe change list asks for a deploy token to be minted.");
    const input = { text: DOCUMENT, documentId: "artifact-1", documentVersion: 3 };
    const options = { ask: asked.ask, definition: DEFINITION, cacheDirectory: directory };
    const first = await neverDecide(input, options);
    const second = await neverDecide(input, options);
    expect(asked.asks).toHaveLength(1);
    expect(first.source).toBe("model");
    expect(second.source).toBe("cache");
    expect({ held: second.held, kind: second.kind, reason: second.reason }).toEqual({
      held: first.held,
      kind: first.kind,
      reason: first.reason,
    });
    // Nothing was paid for the second one, and the whole answer is kept rather than the verdict.
    expect([second.inputTokens, second.outputTokens]).toEqual([null, null]);
    expect(second.answer).toBe(first.answer);
    expect(await readdir(directory)).toHaveLength(1);
  });

  test("asks again when the prose changes, and again when the document's version does", async () => {
    const directory = await scratch();
    const asked = fake("credential\nThe change list asks for a deploy token to be minted.");
    const input = { text: DOCUMENT, documentId: "artifact-1", documentVersion: 3 };
    await neverDecide(input, { ask: asked.ask, definition: DEFINITION, cacheDirectory: directory });
    await neverDecide(input, {
      ask: asked.ask,
      definition: `${DEFINITION} And one more sentence.`,
      cacheDirectory: directory,
    });
    await neverDecide(
      { ...input, documentVersion: 4 },
      { ask: asked.ask, definition: DEFINITION, cacheDirectory: directory }
    );
    expect(asked.asks).toHaveLength(3);
    expect(await readdir(directory)).toHaveLength(3);
  });

  test("names every kind it can hold, the waiting root among them", () => {
    expect([...NEVER_DECIDE_KINDS].sort()).toEqual([
      "changes-these-skills",
      "credential",
      "destructive-act",
      "outside-account",
      "outside-message",
      "owner-only-setting",
      "waiting-legion-root",
    ]);
  });
});
