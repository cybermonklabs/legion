import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { KeymapProvider } from "../shell/KeymapProvider";
import { presetPath } from "./issue-address";
import { framesByHand } from "./outline-fixtures";
import { CONTROL_LABELS } from "./page-words";
import { useIssueKeymap } from "./useIssueKeymap";

const ISSUE = "CORE-1";

/** The issue page's chords (`useIssueKeymap`), the outline with its one row, which holds the
 *  tab stop, and a field elsewhere on the page. */
function ChordPage(): ReactNode {
  useIssueKeymap(ISSUE);
  return (
    <>
      <div aria-label={CONTROL_LABELS.tree} role="tree" tabIndex={-1}>
        <div aria-level={1} data-node-row="" role="treeitem" tabIndex={0}>
          The first row.
        </div>
      </div>
      <input aria-label="Elsewhere" />
    </>
  );
}

// A view's chord opens its address and puts the focus on the outline's tab stop a frame on, once
// the address has drawn the outline, looking again each frame for up to 20. A reader who acts in
// those frames - a press in another field, and words typed there - has put the focus where they
// want it, and a late frame that took it to the row would leave the rest of their keys there,
// where the outline's keys read them. Here every frame is held until the reader has typed, as a
// loaded machine holds them.
test("a reader who moves to another field before the chord's frames keeps the focus and the words typed there", async () => {
  const user = userEvent.setup();
  const page = render(
    <MemoryRouter initialEntries={[presetPath(ISSUE, "conversation")]}>
      <KeymapProvider>
        <ChordPage />
      </KeymapProvider>
    </MemoryRouter>
  );
  const frames = framesByHand();
  try {
    await user.keyboard("ts");
    const elsewhere = screen.getByRole<HTMLInputElement>("textbox", { name: "Elsewhere" });
    await user.click(elsewhere);
    await user.keyboard("ab");
    // The chord's frame: the outline is drawn already, so the part is found in it.
    await frames.run();
    expect(document.activeElement, "the chord's frames took the focus from the other field").toBe(
      elsewhere
    );
    expect(elsewhere.value).toBe("ab");
  } finally {
    frames.restore();
    page.unmount();
  }
});
