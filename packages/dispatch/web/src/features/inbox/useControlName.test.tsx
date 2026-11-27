import { afterEach, beforeAll, expect, test } from "bun:test";
import { cleanup, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import { answerQuestionOf, NAMED_QUESTION } from "../../__tests__/question-answers";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import type { NamedAsk } from "./ask-name";
import { useControlName } from "./useControlName";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

/** A control named as an Inbox row's controls are. */
function Named({ ask }: { ask: NamedAsk }): ReactNode {
  const { name, reads } = useControlName(ask, undefined);
  return (
    <>
      {reads}
      <button aria-label={name} type="button" />
    </>
  );
}

// A control's name carries its ask's question, and the ask is the record the question is, so a
// `#id` in it names the question of the ask's document, as it does on the card. Handed no
// record, the name keeps `#q2`.
test("a #id in an Inbox row's question names the question of its ask in the controls' name", async () => {
  const answers = answerQuestionOf("ask", "ask-7");
  const ask = {
    document: undefined,
    id: "ask-7",
    issue: { key: "CORE-1", title: "Ship the cache" },
    issue_key: "CORE-1",
    question: "Decide #q2 now?",
  } as unknown as NamedAsk;
  const view = render(
    <MemoryRouter>
      <Named ask={ask} />
    </MemoryRouter>
  );
  try {
    expect(
      await screen.findByRole("button", { name: `CORE-1: Decide ${NAMED_QUESTION} now?` })
    ).toBeDefined();
  } finally {
    view.unmount();
    answers.restore();
  }
});
