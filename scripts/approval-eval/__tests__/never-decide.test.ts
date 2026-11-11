import { describe, expect, test } from "bun:test";
import type { CaseRow } from "../build-corpus";
import { type IssueFacts, NEVER_DECIDE_KINDS, neverDecide } from "../never-decide";

/**
 * One case per kind the agent never decides, and three ordinary documents it does decide. The
 * text here is hand-written: nothing reads a corpus and nothing reads Dispatch.
 */

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

/** The issue facts of a parked root carrying the `legion` label. */
function parkedRoot(overrides: Partial<IssueFacts> = {}): IssueFacts {
  return {
    parent: null,
    labels: ["legion"],
    status: "backlog",
    primaryArtifactId: "artifact-1",
    ...overrides,
  };
}

/** One document of each kind the rule holds, with the signal each is meant to raise. */
const held: ReadonlyArray<{ kind: string; text: string; signal: RegExp }> = [
  {
    kind: "destructive-act",
    text: "# A clean-up\n\nThe step runs `rm -rf /var/lib/store` before the restore.\n",
    signal: /rm -rf/,
  },
  {
    kind: "credential",
    text: "# A new app\n\nYou create the app and paste its api-key into the store.\n",
    signal: /make or hand over a credential/,
  },
  {
    kind: "owner-only-setting",
    text: "# An install\n\nOnly an organization owner can add the repository to the App.\n",
    signal: /only an owner can make/,
  },
  {
    kind: "outside-message",
    text: "# A report\n\nYou email the customer with the dates once the run is over.\n",
    signal: /customer, a vendor or an upstream project/,
  },
  {
    kind: "outside-account",
    text: "# A platform\n\nYou register an account with the provider before the first run.\n",
    signal: /open an account/,
  },
  {
    kind: "changes-these-skills",
    text: "# A change\n\nIt rewrites the never-decide rule and the number it records.\n",
    signal: /never-decide rule/,
  },
];

describe("neverDecide", () => {
  for (const one of held) {
    test(`holds ${one.kind} and names the signal`, () => {
      const answer = neverDecide({ text: one.text, row: caseRow() });
      expect(answer.held).toBe(true);
      expect(answer.kind).toBe(one.kind as (typeof NEVER_DECIDE_KINDS)[number]);
      expect(answer.reason).toMatch(one.signal);
    });
  }

  test("holds the primary document of a parked root carrying the legion label", () => {
    const answer = neverDecide({
      text: "# A root\n\nIt says what the tree will do.\n",
      row: caseRow(),
      issue: parkedRoot(),
    });
    expect(answer.held).toBe(true);
    expect(answer.kind).toBe("parked-legion-root");
    expect(answer.reason).toMatch(/root carrying the legion label, parked at backlog/);
  });

  test("leaves a parked legion issue that is not a root, or not primary, or not parked", () => {
    const text = "# A root\n\nIt says what the tree will do.\n";
    const kinds = [
      parkedRoot({ parent: "PROJ-9" }),
      parkedRoot({ labels: [] }),
      parkedRoot({ status: "in_progress" }),
      parkedRoot({ primaryArtifactId: "artifact-2" }),
    ];
    for (const issue of kinds) {
      expect(neverDecide({ text, row: caseRow(), issue }).held).toBe(false);
    }
  });

  test("holds no case under the parked kind when the caller names no issue facts", () => {
    const answer = neverDecide({ text: "# A root\n\nIt says what it does.\n", row: caseRow() });
    expect(answer.held).toBe(false);
  });

  test("decides three ordinary documents", () => {
    const ordinary = [
      "# A rename\n\nThe field `issue_key` becomes `issueKey` in the three readers of it.\n",
      "# A test\n\nIt adds one table-driven test over the parser, and changes no behaviour.\n",
      "# A report\n\nThe daemon prints how long each step took, in one line per step.\n",
    ];
    for (const text of ordinary) {
      const answer = neverDecide({ text, row: caseRow() });
      expect(answer.held).toBe(false);
      expect(answer.kind).toBeNull();
      expect(answer.reason).toBe("no kind the agent never decides was named");
    }
  });

  test("names every kind it can hold, the parked root among them", () => {
    expect([...NEVER_DECIDE_KINDS].sort()).toEqual([
      "changes-these-skills",
      "credential",
      "destructive-act",
      "outside-account",
      "outside-message",
      "owner-only-setting",
      "parked-legion-root",
    ]);
  });
});
