import { afterEach, beforeAll, expect, spyOn } from "bun:test";
import type { EditorReferencePicker, ReferenceInsertion } from "@legion/proof-editor";
import { act, cleanup, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import {
  architectureTree,
  issueCollaborators,
  plannerAgent,
  searchResponse,
} from "../../__tests__/search-fixture";
import { api } from "../../api/client";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { refPickerTest as test } from "../refs/ref-picker-fixture";
import { DocumentRefPicker } from "./DocumentRefPicker";
import type { EditorHandle } from "./editor";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * The reference picker in a document's body, over the editor's side of it: the text the editor
 * reports before the caret, the keys it offers, and the pick it is handed to write.
 */

afterEach(cleanup);

/** The editor as the host sees it: what it was handed to host the picker with, and each pick it
 *  was asked to write. */
function fakeEditor() {
  const seen: { picker: EditorReferencePicker | null; insertions: ReferenceInsertion[] } = {
    insertions: [],
    picker: null,
  };
  const editor = {
    insertReference(insertion: ReferenceInsertion) {
      seen.insertions.push(insertion);
      return true;
    },
    setReferencePicker(picker: EditorReferencePicker | null) {
      seen.picker = picker;
    },
  } as unknown as EditorHandle;
  return { editor, seen };
}

// A document's links are marks: the address of one is the mark's, never characters of the text
// the editor reports, so a `](` the reader typed in it is only those two characters. A party
// picked after them is still the mention the document stores as a link, not a bare address
// written as plain text.
test("a party picked after a `](` typed in a document is its mention", async () => {
  const restore = [
    spyOn(api, "listAgents").mockResolvedValue([plannerAgent()]),
    spyOn(api, "getIssueCollaborators").mockResolvedValue(issueCollaborators()),
    spyOn(api, "getArchitecture").mockResolvedValue(architectureTree()),
    spyOn(api, "search").mockResolvedValue(searchResponse([])),
  ];
  const { editor, seen } = fakeEditor();
  try {
    render(
      <MemoryRouter>
        <DocumentRefPicker editor={editor} enabled issueKey="CORE-1" />
      </MemoryRouter>
    );
    await waitFor(() => expect(seen.picker).not.toBeNull());
    // One letter after the door, below what the search route takes, so the rows are the parties.
    const text = "[the plan](@p";
    act(() => seen.picker?.onCaret({ bottom: 20, left: 0, text, top: 10 }));
    await screen.findByText("Planner");
    act(() => {
      expect(seen.picker?.pressKey("Enter")).toBe(true);
    });

    expect(seen.insertions).toEqual([
      {
        end: text.length,
        href: "dispatch://agent/planner-session",
        start: "[the plan](".length,
        text: "@Planner",
        trigger: "@",
      },
    ]);
  } finally {
    for (const spy of restore) spy.mockRestore();
  }
});
