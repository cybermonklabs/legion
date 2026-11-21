import { expect, test } from "bun:test";

import { graphNode } from "../../__tests__/graph-fixture";
import type { DecisionLink, IssueAskRow, IssueDecisions } from "../../api/types";
import { decisionBlockId, decisionDocumentId } from "../issue-page/decision-review";
import { type DecisionsRead, isDecisionEntry, withEntries } from "./decision-entries";

const KEY = "CORE-1";

const link: DecisionLink = {
  created_at: "2026-11-01T00:00:00Z",
  created_version: 1,
  from_block: "h-store",
  stale: true,
  to: graphNode({ block_id: "p1", id: `${KEY}/spec#p1`, kind: "block" }),
};

/** A Decisions read as a server of the release before answers it: no `entries`, one open
 *  question, and two sections of the issue's decisions document, the agent's and a person's. */
const before: DecisionsRead = {
  decisions: [
    {
      asks: ["ask-settled"],
      block_id: "h-store",
      changed_at: "2026-11-01T00:00:00Z",
      changed_version: 3,
      heading: "Store notices in a table",
      kept: null,
      kind: "agent",
      links: [link],
      ref: `dispatch://${KEY}/artifact/decisions-md#b-h-store`,
      replaced: false,
      text: "## Store notices in a table\n\nA table keeps them.",
      text_sha256: "sum-store",
      unsure: true,
      waiting_review: true,
    },
    {
      asks: ["ask-named"],
      block_id: "h-name",
      changed_at: null,
      changed_version: 2,
      heading: "Name them notices",
      kept: null,
      kind: "person",
      links: [],
      ref: `dispatch://${KEY}/artifact/decisions-md#b-h-name`,
      replaced: false,
      text: "## Name them notices\n\nThe person chose the name.",
      text_sha256: "sum-name",
      unsure: false,
      waiting_review: false,
    },
  ],
  document: `dispatch://${KEY}/artifact/decisions-md`,
  open_questions: [
    {
      id: "ask-open",
      issue_key: null,
      multiple: true,
      options: [{ label: "A btree" }],
      question: "Which index?",
      urgency: "blocking",
    } as unknown as IssueAskRow,
  ],
};

// This build's server answers `entries`, and they are what every view reads: the read is handed
// on with them as they came.
test("a read that carries entries keeps them as they came", () => {
  const read: IssueDecisions = { decisions: [], document: null, entries: [], open_questions: [] };
  expect(withEntries(read, KEY).entries).toBe(read.entries);
});

// A server of the release before answers no `entries`: its open questions and its sections are
// read as the entries they are, so a view of this build draws what that server answered.
test("a read of the release before is read as entries of its open questions and its decisions", () => {
  const entries = withEntries(before, KEY).entries ?? [];
  expect(entries.map((entry) => [entry.ref, entry.kind, entry.answer_state])).toEqual([
    [`dispatch://${KEY}/ask/ask-open`, "person", "open"],
    [`dispatch://${KEY}/artifact/decisions-md#b-h-store`, "agent", null],
    [`dispatch://${KEY}/artifact/decisions-md#b-h-name`, "person", "answered"],
  ]);
  const [question, agent, person] = entries;
  expect(question).toMatchObject({
    from_ask: [],
    multiple: true,
    node: { id: "ask-open", kind: "ask" },
    options: [{ label: "A btree" }],
    question: "Which index?",
    urgency: "blocking",
  });
  expect(agent).toMatchObject({
    document: null,
    from_ask: ["ask-settled"],
    shaped: [link],
    text_sha256: "sum-store",
    uncertainty: "high",
    waiting_review: true,
  });
  expect(entries.map((entry) => (entry === undefined ? false : isDecisionEntry(entry)))).toEqual([
    false,
    true,
    true,
  ]);
  // The section's address names the block a Keep is written against and the document it stands in.
  if (agent === undefined || person === undefined) throw new Error("the sections are no entries");
  expect(decisionBlockId(agent)).toBe("h-store");
  expect(
    decisionDocumentId(
      {
        artifacts: [{ id: "decisions-id", slug: "decisions-md" }],
        key: KEY,
        primary_artifact_id: "spec-id",
      },
      person
    )
  ).toBe("decisions-id");
});
