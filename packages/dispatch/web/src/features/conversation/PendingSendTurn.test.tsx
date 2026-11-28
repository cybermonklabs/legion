import { afterEach, beforeAll, expect, test } from "bun:test";
import { cleanup, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import type { HeldSend } from "./held-sends";
import { PendingSendTurn } from "./PendingSendTurn";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

// A held message is a write the server has not stored yet, so the page draws it through the
// renderer, as the message it becomes will be drawn: its paragraphs stay paragraphs. Drawn as
// one line of plain words, the turn would run both paragraphs into one.
test("a held message keeps its line breaks, drawn as the message it becomes", async () => {
  const held = {
    draft: { draft: { body: "First **line**.\n\nSecond line." } },
    id: 1,
    signingOutElsewhere: false,
    status: "waiting",
  } as unknown as HeldSend;
  const view = render(
    <MemoryRouter>
      <PendingSendTurn held={held} sends={{ discard: () => {}, retry: () => {}, stop: () => {} }} />
    </MemoryRouter>
  );
  try {
    const turn = view.getByTestId("agent-send-held");
    await waitFor(() =>
      expect([...turn.querySelectorAll(".dispatch-markdown p")].map((p) => p.textContent)).toEqual([
        "First line.",
        "Second line.",
      ])
    );
    expect(turn.querySelector("strong")?.textContent).toBe("line");
  } finally {
    view.unmount();
  }
});
