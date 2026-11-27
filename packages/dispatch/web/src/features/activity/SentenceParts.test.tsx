import { afterEach, beforeAll, expect, test } from "bun:test";
import { cleanup, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import { answerQuestionOf, NAMED_ASK, NAMED_QUESTION } from "../../__tests__/question-answers";
import type { DispatchEvent } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { activityDescription } from "./activity-model";
import { SentenceParts } from "./SentenceParts";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

const ASK_ID = "ask-1";

/** An `ask.opened` event of CORE-1 whose question is `question`. */
function opened(question: string): DispatchEvent {
  return {
    actor: { id: "planner", kind: "session" },
    created_at: "2026-09-10T10:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      answer: null,
      artifact_id: null,
      author: { id: "planner", kind: "session" },
      block_id: null,
      created_at: "2026-09-10T10:00:00Z",
      id: ASK_ID,
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      question,
      state: "open",
      urgency: "med",
    },
    seq: 1,
    type: "ask.opened",
  } as unknown as DispatchEvent;
}

// An event's line is in no link or button, so the question it quotes is drawn by the renderer
// as the ask's own record: a `#id` in it names the question of the ask's document and is a link
// reading as that question, as it is on the ask's card. Drawn as the projection with no record,
// the line would keep `#q2` as plain words.
test("a #id in the question an event's line quotes is a link reading as the question it names", async () => {
  const answers = answerQuestionOf("ask", ASK_ID);
  const view = render(
    <MemoryRouter>
      <p>
        <SentenceParts parts={activityDescription(opened("Decide #q2 now."))} />
      </p>
    </MemoryRouter>
  );
  try {
    await waitFor(() => {
      const link = view.container.querySelector<HTMLAnchorElement>(
        `a[data-dispatch-ref="${NAMED_ASK}"]`
      );
      expect(link?.textContent).toBe(NAMED_QUESTION);
      expect(link?.getAttribute("href")).toBeTruthy();
    });
    expect(view.container.textContent).toBe(`asked “Decide ${NAMED_QUESTION} now.”`);
  } finally {
    view.unmount();
    answers.restore();
  }
});
