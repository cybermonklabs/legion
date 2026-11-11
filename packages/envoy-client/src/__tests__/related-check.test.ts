import { describe, expect, test } from "bun:test";
import type { AdviceDeclinedAsk, SessionAnswer, WriteAdvice } from "@legion/contracts";
import { declinedAskLines, type RelatedCheckContext } from "../related-check";
import { renderSessionAnswerItem } from "../session-answers";

/** The four sentences Dispatch writes beside a person's note when they decline to answer
 *  (`model.DeclineDispatchWords`). Every reader of a decline prints them, so they are written
 *  here as the server writes them. */
const DISPATCH_WORDS =
  "The person chose not to answer this question. Decide the next step without an answer. " +
  "Do not ask this question again unless the facts change. Write in your work what you assumed.";

const context: RelatedCheckContext = {
  project: "CORE",
  text: "Which transport ships first?",
  configUrl: "http://dispatch.test",
  issueKey: "CORE-7",
};

/** A write's advice as the ask route and the document-edit route answer it, carrying the
 *  issue's declined asks when the server read them and nothing where it sent none. */
function advice(declined?: readonly AdviceDeclinedAsk[]): WriteAdvice {
  return {
    decision_blocks: 0,
    decision_blocks_added: 0,
    issue_status: "in_progress",
    session_writes_since_human: 0,
    your_open_asks: null,
    ...(declined === undefined ? {} : { declined_asks: [...declined] }),
  };
}

const transport: AdviceDeclinedAsk = {
  id: "ask-1",
  question: "Which transport ships first?",
  note: "Whoever builds it decides.",
};
const region: AdviceDeclinedAsk = {
  id: "ask-2",
  question: "Which region goes first?",
  note: "",
};

describe("what a write about to ask is told of the issue's declined asks", () => {
  test("a declined ask of the issue is printed as declined and never as answered", () => {
    const lines = declinedAskLines(advice([transport, region]), context);

    expect(lines).toEqual([
      "This issue holds 2 questions a person declined to answer. Ask one again only if the facts have changed:",
      "- Declined: Which transport ships first? → http://dispatch.test/issues/CORE-7/asks/ask-1",
      "  Their note: Whoever builds it decides.",
      "- Declined: Which region goes first? → http://dispatch.test/issues/CORE-7/asks/ask-2",
    ]);
    // A decline is not an answer: a reader that took one for the other would stop waiting on a
    // person who said nothing.
    expect(lines.join("\n")).not.toMatch(/answered|answer:/iu);

    expect(declinedAskLines(advice([transport]), context)[0]).toBe(
      "This issue holds a question a person declined to answer. Ask it again only if the facts have changed:"
    );
    // A context naming no issue - a write on a project document - has no page to link, so the
    // line names the ask by the id every ask command takes.
    expect(declinedAskLines(advice([region]), { ...context, issueKey: undefined })).toEqual([
      "This issue holds a question a person declined to answer. Ask it again only if the facts have changed:",
      "- Declined: Which region goes first? (ask ask-2)",
    ]);

    // A person's note is bounded only by the event body cap, and the field names every declined
    // ask of the issue, so the line cuts the note as it cuts the question; the whole of it is at
    // the link the line above prints.
    const wordy: AdviceDeclinedAsk = {
      id: "ask-3",
      question: "Which queue?",
      note: `${"word ".repeat(80)}end`,
    };
    const cut = `${"word ".repeat(80).trim().slice(0, 200)}…`;
    expect(declinedAskLines(advice([wordy]), context)[2]).toBe(`  Their note: ${cut}`);
  });

  test("a write the server sent no declined asks on prints nothing, and a broken one prints why", () => {
    expect(declinedAskLines(undefined, context)).toEqual([]);
    expect(declinedAskLines(advice(), context)).toEqual([]);
    expect(declinedAskLines(advice([]), context)).toEqual([]);

    // The lines ride on a write that already succeeded, so a result this client cannot read
    // costs the reader these lines and nothing above them.
    const unreadable = {
      get declined_asks(): readonly AdviceDeclinedAsk[] {
        throw new Error("the advice could not be read");
      },
    } as unknown as WriteAdvice;

    expect(declinedAskLines(unreadable, context)).toEqual([
      "The issue's declined asks could not be shown: the advice could not be read",
    ]);
  });

  // The server-side check proves what `GET /api/v1/agents/{session_id}/answers` answers, not
  // what the session reads: this is the line `dispatch answers` and the per-request list print.
  test("dispatch answers prints a decline with the person's note apart from Dispatch's words", () => {
    const declined: SessionAnswer = {
      kind: "declined",
      relation: "declined_your_ask",
      id: "ask-1",
      at: "2026-10-27T11:00:00Z",
      author: { kind: "user", id: "ana@example.com" },
      text: "Whoever builds it decides.",
      dispatch_words: DISPATCH_WORDS,
      ask: { id: "ask-1", question: "Which transport ships first?" },
      owner: { issue: { key: "CORE-7", title: "One transport for the service" } },
      ref: "dispatch://CORE-7/ask/ask-1",
      href: "/issues/CORE-7/asks/ask-1",
    };

    expect(renderSessionAnswerItem(declined)).toBe(
      [
        "2026-10-27T11:00:00Z · user ana@example.com · declined to answer your ask",
        "dispatch://CORE-7/ask/ask-1 · CORE-7: One transport for the service",
        "Question: Which transport ships first?",
        `Dispatch's words: ${DISPATCH_WORDS}`,
        "Whoever builds it decides.",
      ].join("\n")
    );

    // A decline with no note is the ordinary one: Dispatch's words stand alone, with no empty
    // line where the person's note would be.
    expect(renderSessionAnswerItem({ ...declined, text: "" })).toBe(
      [
        "2026-10-27T11:00:00Z · user ana@example.com · declined to answer your ask",
        "dispatch://CORE-7/ask/ask-1 · CORE-7: One transport for the service",
        "Question: Which transport ships first?",
        `Dispatch's words: ${DISPATCH_WORDS}`,
      ].join("\n")
    );
  });
});
