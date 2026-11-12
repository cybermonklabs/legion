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

/** One replayed document as a test names it: its name, its artifact, its version and its text. */
function documentNamed(
  name: string,
  artifactId: string | null,
  requestedVersion: number | null,
  markdown: string | null
): ReplayedDocument {
  return { ...document(markdown), name, artifactId, version: requestedVersion, requestedVersion };
}

/** One case asking about the documents named, as `replayCase` hands them to the rendering. */
function caseOver(
  documents: readonly ReplayedDocument[],
  overrides: Partial<CaseRow> = {}
): CaseRow {
  return caseRow({
    approval: {
      documents: documents.map((one) => ({
        artifactId: one.artifactId,
        name: one.name,
        version: one.version,
        requestedVersion: one.requestedVersion,
      })),
      artifactSlug: "spec",
      artifactPrimary: true,
    },
    ...overrides,
  });
}

/** A document's body, with the line a test puts in it and text after it, so no trailer is cut. */
function bodyQuoting(line: string): string {
  return ["# A document", "", line, "", "The rest of it."].join("\n");
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

  test("throws on an approval at or above the version the head of a rendering is held to", () => {
    const row = caseRow();
    expect(() => {
      assertNoLeak(row, "Approval: approved v7 by a person\n");
    }).toThrow(LeakError);
    expect(() => {
      assertNoLeak(row, "Approval: approved v8 by a person\n");
    }).toThrow(/at or above the lowest version the request asked about, 7/);
    expect(() => {
      assertNoLeak(row, "Approval: approved v6 by a person\n");
    }).not.toThrow();
  });

  /**
   * The shape of case a39f4947-8990-487f-b0b7-e00531273575 of the recorded round, which the
   * lowest-version rule refused and no rule before it did. Its spec.md, asked about at version
   * 23, carries in its own body a line reading `Approval: approved v13 by <a person>, changed
   * since (now v15)`, and the verification.md of the same request was asked about at version 7.
   * Versions count separately for each artifact, so v13 of spec.md is no answer to a request
   * asking about v23 of spec.md, and it says nothing whatever about verification.md.
   */
  test("passes a document quoting an approval of an earlier version of itself", () => {
    const documents = [
      documentNamed(
        "spec.md",
        "artifact-1",
        23,
        bodyQuoting("Approval: approved v13 by a person, changed since (now v15)")
      ),
      documentNamed("verification.md", "artifact-2", 7, "# Verification\n\nIt was checked."),
    ];
    const row = caseOver(documents);
    expect(() => {
      assertNoLeak(row, renderCase(row, documents));
    }).not.toThrow();
  });

  test("throws on a document quoting an approval at or above its own requested version", () => {
    const documents = [
      documentNamed("spec.md", "artifact-1", 13, bodyQuoting("Approval: approved v13 by a person")),
      documentNamed("verification.md", "artifact-2", 7, "# Verification\n\nIt was checked."),
    ];
    const row = caseOver(documents);
    expect(() => {
      assertNoLeak(row, renderCase(row, documents));
    }).toThrow(LeakError);
    expect(() => {
      assertNoLeak(row, renderCase(row, documents));
    }).toThrow(/approval of version 13, at or above the version 13 of spec\.md/);
  });

  test("passes a document naming only a version below its own", () => {
    const documents = [
      documentNamed("spec.md", "artifact-1", 13, bodyQuoting("Approval: approved v12 by a person")),
    ];
    const row = caseOver(documents);
    expect(() => {
      assertNoLeak(row, renderCase(row, documents));
    }).not.toThrow();
  });

  /**
   * A document naming no artifact has its own `requestedVersion` read all the same, which is
   * what the lead-document rule got wrong and the lowest-version rule was right about: the
   * corpus holds requests whose version-bearing document names no artifact.
   */
  test("holds a document naming no artifact to its own requested version", () => {
    const leaking = [
      documentNamed("notes.md", null, 9, bodyQuoting("Approval: approved v9 by a person")),
      documentNamed("spec.md", "artifact-1", 3, "# A spec\n\nIt says what it says."),
    ];
    const row = caseOver(leaking);
    expect(() => {
      assertNoLeak(row, renderCase(row, leaking));
    }).toThrow(/at or above the version 9 of notes\.md/);
    const clean = [
      documentNamed("notes.md", null, 9, bodyQuoting("Approval: approved v8 by a person")),
      documentNamed("spec.md", "artifact-1", 3, "# A spec\n\nIt says what it says."),
    ];
    const other = caseOver(clean);
    expect(() => {
      assertNoLeak(other, renderCase(other, clean));
    }).not.toThrow();
  });

  test("holds a document the request names no version for to the lowest asked about", () => {
    const documents = [
      documentNamed("spec.md", "artifact-1", null, bodyQuoting("Approval: approved v7 by them")),
      documentNamed("notes.md", null, 7, "# Notes\n\nIt notes what it notes."),
    ];
    const row = caseOver(documents);
    expect(() => {
      assertNoLeak(row, renderCase(row, documents));
    }).toThrow(/at or above the lowest version the request asked about, 7/);
  });

  test("holds the question, which belongs to no document, to the lowest asked about", () => {
    const documents = [
      documentNamed("spec.md", "artifact-1", 23, "# A spec\n\nIt says what it says."),
      documentNamed("verification.md", "artifact-2", 7, "# Verification\n\nIt was checked."),
    ];
    const row = caseOver(documents, {
      question: "Approve spec.md? Approval: approved v9 by a person stands on it.",
    });
    expect(() => {
      assertNoLeak(row, renderCase(row, documents));
    }).toThrow(/at or above the lowest version the request asked about, 7/);
  });

  /**
   * The documents rendered are not the row's: `fresh-agent.ts` takes the row from `--corpus`
   * and the documents from a round's cache, and that cache is read on the case id and the cache
   * version alone, so a renamed second document leaves the heading the row names unwritten. The
   * body under the renamed one is no stretch of the first document, and holding it to the first
   * document's version is weaker than the rule over the whole text: at a lowest of 3 it let an
   * approval of v3 through.
   */
  test("holds a rendering of documents that are not the row's to the lowest version", () => {
    const asked = documentNamed("a.md", "artifact-1", 9, "# A\n\nIt says what it says.");
    const row = caseOver([asked, documentNamed("b.md", "artifact-2", 3, "# B\n\nIt was checked.")]);
    const cached = [
      asked,
      documentNamed(
        "renamed.md",
        "artifact-2",
        3,
        bodyQuoting("Approval: approved v3 by a person")
      ),
    ];
    expect(() => {
      assertNoLeak(row, renderCase(row, cached));
    }).toThrow(/at or above the lowest version the request asked about, 3/);
  });

  /**
   * No mismatch is needed for the same weakening: a line of a document's own body that reads
   * like the next document's heading cuts the section there, and the rest of that body is held
   * to the next document's version, which here is the higher one. The stretch is still the
   * first document's text, so the lowest version is what it is held to.
   */
  test("holds a rendering whose body reads like the next heading to the lowest version", () => {
    const next = "## Document 2 of 2: b.md (version 9)";
    const documents = [
      documentNamed(
        "a.md",
        "artifact-1",
        3,
        ["# A", "", next, "", "Approval: approved v3 by a person", "", "The rest of it."].join("\n")
      ),
      documentNamed("b.md", "artifact-2", 9, "# B\n\nIt was checked."),
    ];
    const row = caseOver(documents);
    expect(() => {
      assertNoLeak(row, renderCase(row, documents));
    }).toThrow(/at or above the lowest version the request asked about, 3/);
  });

  test("holds no version rule over a request naming no version at all", () => {
    const row = caseRow({
      approval: {
        documents: [
          { artifactId: "artifact-1", name: "spec.md", version: null, requestedVersion: null },
        ],
        artifactSlug: "spec",
        artifactPrimary: true,
      },
    });
    expect(() => {
      assertNoLeak(row, "Approval: approved v9 by a person\n");
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
