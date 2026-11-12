import { describe, expect, test } from "bun:test";
import type { CaseRow } from "../build-corpus";
import { assertNoLeak, LeakError, renderCase, renderDocument } from "../render";
import type { ReplayedDocument } from "../replay";

/**
 * The four rules that keep the person's answer out of the evidence, over hand-written markdown:
 * nothing here reads a corpus from disk and nothing here reads Dispatch.
 */

const ANSWERED_AT = "2026-11-06T12:00:00Z";

/** One approval case, with only the fields the rendering reads named. */
function caseRow(overrides: Partial<CaseRow> = {}): CaseRow {
  return {
    caseId: "case-1",
    eventId: 1,
    askId: "ask-1",
    issueKey: "PROJ-1",
    project: "PROJ",
    kind: "approval",
    urgency: "med",
    multiple: false,
    question: "Approve spec.md (version 7)?",
    options: [
      { id: "b2", label: "Approve", description: "Approve this version of the document." },
      { id: "b3", label: "Request changes", description: "Say what must change." },
    ],
    selected: ["Approve"],
    words: "Looks right to me.",
    hasWords: true,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: ANSWERED_AT,
    eventCreatedAt: ANSWERED_AT,
    approval: {
      documents: [{ artifactId: "artifact-1", name: "spec.md", version: 7, requestedVersion: 7 }],
      artifactSlug: "spec",
      artifactPrimary: true,
    },
    verdict: "approve",
    confounds: {
      version: 7,
      answersThatDayBefore: 0,
      hourUtc: 12,
      hourPacific: 4,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: "approval",
    },
    ...overrides,
  };
}

/** One replayed document, carrying the markdown a test hands it. */
function document(markdown: string | null, refusal: string | null = null): ReplayedDocument {
  return {
    name: "spec.md",
    artifactId: "artifact-1",
    version: 7,
    requestedVersion: 7,
    markdown,
    bytes: markdown === null ? null : markdown.length,
    sha256: null,
    versionCreatedAt: "2026-11-05T08:00:00Z",
    named: false,
    summary: null,
    refusal,
  };
}

/** An ask block's fence, in the syntax a document version carries it in. */
function fence(attributes: string): string {
  return `:::ask{#b1 urgency="med" multiple="false" ${attributes}}`;
}

describe("renderDocument", () => {
  test("strips Dispatch's trailer lines from the end of the markdown", () => {
    const markdown = [
      "# A spec",
      "",
      "It says what it says.",
      "",
      "Document token: sha256:abc Approval: approved v6 by a person",
      "",
    ].join("\n");
    const rendered = renderDocument(markdown, ANSWERED_AT);
    expect(rendered).toBe("# A spec\n\nIt says what it says.");
    expect(rendered).not.toContain("Document token:");
    expect(rendered).not.toContain("Approval:");
  });

  test("leaves a trailer-looking line alone inside the body", () => {
    const markdown = ["# A spec", "", "Approval: approved v2 by a person", "", "The rest."].join(
      "\n"
    );
    expect(renderDocument(markdown, ANSWERED_AT)).toContain("Approval: approved v2");
  });

  test("keeps an ask block whose answer is earlier than this case's answer", () => {
    const markdown = [
      "# A spec",
      "",
      fence(
        'state="answered" answered_by="a-person" answered_at="2026-11-04T18:00:00Z" ' +
          'selected="[&#x22;Leave them out&#x22;]"'
      ),
      "Which way?",
      ":::",
    ].join("\n");
    const rendered = renderDocument(markdown, ANSWERED_AT);
    expect(rendered).toContain('answered_at="2026-11-04T18:00:00Z"');
    expect(rendered).toContain('state="answered"');
    expect(rendered).toContain("Leave them out");
  });

  test("strips the answer of an ask block answered at or after this case's answer", () => {
    for (const at of [ANSWERED_AT, "2026-11-07T09:00:00Z"]) {
      const markdown = [
        "# A spec",
        "",
        fence(
          `state="answered" answered_by="a-person" answered_at="${at}" ` +
            'selected="[&#x22;Take the second&#x22;]" answer="Because I said so."'
        ),
        "Which way?",
        ":::",
      ].join("\n");
      const rendered = renderDocument(markdown, ANSWERED_AT);
      expect(rendered).not.toContain("answered_at");
      expect(rendered).not.toContain("answered_by");
      expect(rendered).not.toContain("selected");
      expect(rendered).not.toContain("Because I said so.");
      expect(rendered).toContain('state="open"');
      expect(rendered).toContain('urgency="med"');
      expect(rendered).toContain("Which way?");
    }
  });

  test("strips the answer of an ask block whose answer carries no readable date", () => {
    const markdown = fence('state="answered" answered_by="a-person" selected="[&#x22;One&#x22;]"');
    const rendered = renderDocument(markdown, ANSWERED_AT);
    expect(rendered).toBe(':::ask{#b1 urgency="med" multiple="false" state="open"}');
  });
});

describe("renderCase", () => {
  test("carries the issue, the project, the question and the options", () => {
    const rendered = renderCase(caseRow(), [document("# A spec\n\nIt says what it says.")], {
      issueTitle: "A title",
    });
    expect(rendered).toContain("Issue: PROJ-1 — A title");
    expect(rendered).toContain("Project: PROJ");
    expect(rendered).toContain("Approve spec.md (version 7)?");
    expect(rendered).toContain("- Approve: Approve this version of the document.");
    expect(rendered).toContain("## Document 1 of 1: spec.md (version 7)");
    expect(rendered).toContain("It says what it says.");
  });

  test("carries no verdict, no answer and none of the person's words", () => {
    const row = caseRow();
    const rendered = renderCase(row, [document("# A spec")], { issueTitle: "A title" });
    expect(rendered).not.toContain(row.words ?? "");
    expect(rendered).not.toMatch(/verdict/i);
    // Two cases differing only in what the person answered render to the same bytes, which is
    // what "the answer is not in the evidence" means.
    const answered = caseRow({
      selected: ["Request changes"],
      verdict: "request_changes",
      words: null,
      hasWords: false,
    });
    expect(renderCase(answered, [document("# A spec")], { issueTitle: "A title" })).toBe(rendered);
  });

  test("says so where a document could not be read", () => {
    const rendered = renderCase(caseRow(), [document(null, "404 on GET the version")]);
    expect(rendered).toContain("could not be read: 404 on GET the version");
  });
});

describe("assertNoLeak", () => {
  test("passes a clean case", () => {
    const rendered = renderCase(caseRow(), [
      document(
        ["# A spec", "", fence('state="answered" answered_at="2026-11-04T18:00:00Z"'), ":::"].join(
          "\n"
        )
      ),
    ]);
    expect(() => {
      assertNoLeak(caseRow(), rendered);
    }).not.toThrow();
  });

  test("throws on an answer dated at or after the case's own answer", () => {
    const row = caseRow();
    const leaking = `Some text\n:::ask{#b1 answered_at="${ANSWERED_AT}"}\n:::\n`;
    expect(() => {
      assertNoLeak(row, leaking);
    }).toThrow(LeakError);
    expect(() => {
      assertNoLeak(row, leaking);
    }).toThrow(/case case-1/);
  });

  test("throws on an approval at or above the case's own requested version", () => {
    const row = caseRow();
    expect(() => {
      assertNoLeak(row, "Approval: approved v7 by a person\n");
    }).toThrow(LeakError);
    expect(() => {
      assertNoLeak(row, "Approval: approved v8 by a person\n");
    }).toThrow(/at or above the requested version 7/);
    expect(() => {
      assertNoLeak(row, "Approval: approved v6 by a person\n");
    }).not.toThrow();
  });

  /**
   * The draw groups a case by `leadDocumentOf`, which takes the first document naming both an
   * artifact and a version. The guard reads that same document, so a document naming a version
   * and no artifact — one the draw never drew by — does not set the version guarded.
   */
  test("guards the version of the document the case is drawn by", () => {
    const row = caseRow({
      approval: {
        documents: [
          { artifactId: null, name: "notes.md", version: 9, requestedVersion: 9 },
          { artifactId: "artifact-1", name: "spec.md", version: 3, requestedVersion: 3 },
        ],
        artifactSlug: "spec",
        artifactPrimary: true,
      },
    });
    expect(() => {
      assertNoLeak(row, "Approval: approved v3 by a person\n");
    }).toThrow(/at or above the requested version 3/);
    expect(() => {
      assertNoLeak(row, "Approval: approved v2 by a person\n");
    }).not.toThrow();
  });

  test("names the offending text on the error", () => {
    const row = caseRow();
    try {
      assertNoLeak(row, "Approval: approved v9 by a person\n");
      throw new Error("the leak was not caught");
    } catch (error) {
      expect(error).toBeInstanceOf(LeakError);
      expect((error as LeakError).caseId).toBe("case-1");
      expect((error as LeakError).offending).toBe("Approval: approved v9");
    }
  });
});
