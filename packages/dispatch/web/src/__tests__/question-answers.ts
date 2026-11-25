import { spyOn } from "bun:test";

import { api } from "../api/client";
import type { AskRead } from "../api/types";

/** The ask a `#q2` names in the rows that answer it here, and the question that ask asks. */
export const NAMED_ASK_ID = "2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b";
export const NAMED_ASK = `dispatch://CORE-1/ask/${NAMED_ASK_ID}`;
export const NAMED_QUESTION = "Ship it?";

const namedAsk = {
  answers: [],
  ask: {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "planner", kind: "session" },
    block_id: "q2",
    created_at: "2026-01-01T00:00:00Z",
    edited_at: null,
    id: NAMED_ASK_ID,
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: NAMED_QUESTION,
    state: "open",
    urgency: "med",
  },
  claimant: null,
  edits: [],
  followers: [],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
} as unknown as AskRead;

/**
 * Answers the reads a `#q2` is named through, for the text of one record alone: that record's
 * question map declares the ask above as `q2`, and the ask's own read says what it asks. Every
 * other record's map declares nothing, so a mount that hands its construct the wrong record, or
 * none, leaves `#q2` as the words written - which is what each row that uses this tells apart.
 */
export function answerQuestionOf(kind: string, id: string): { restore(): void } {
  const questions = spyOn(api, "getReferenceQuestions").mockImplementation(
    async (asked, askedId) =>
      asked === kind && askedId === id
        ? {
            document: "the-document",
            questions: [{ ask: NAMED_ASK_ID, block_id: "q2", reference: NAMED_ASK }],
          }
        : { document: "the-document", questions: [] }
  );
  const ask = spyOn(api, "getAsk").mockResolvedValue(namedAsk);
  return {
    restore: () => {
      questions.mockRestore();
      ask.mockRestore();
    },
  };
}
