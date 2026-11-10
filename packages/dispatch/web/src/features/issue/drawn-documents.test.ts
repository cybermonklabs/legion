import { expect, test } from "bun:test";

import type { Artifact } from "../../api/types";
import type { IssueTab } from "../refs/routes";
import { drawnDocumentIds, drawnDocuments, editedDocument } from "./drawn-documents";

/**
 * One document has one editor and never a second (`document-stage.tsx`). The page and its
 * panels both read `drawnDocuments`, so the rows below hold that one answer to the rule: a
 * document a panel draws never also gets the page's own editor for an edited row, whichever
 * address is open and whichever panels have been opened.
 */

function artifact(id: string, kind: Artifact["kind"] = "doc"): Artifact {
  return {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: "CORE-1",
    kind,
    name: `${id}.md`,
    primary: id === "spec",
    project: "CORE",
    ref_key: `CORE-1/${id}`,
    session_id: null,
    slug: id,
    versions: [],
  };
}

const spec = artifact("spec");
const plan = artifact("plan");
const verification = artifact("verification");
const notes = artifact("notes");
const picture = artifact("picture", "file");

const TABS: readonly IssueTab[] = [
  "activity",
  "artifacts",
  "children",
  "collaborators",
  "conversation",
  "decisions",
  "plan",
  "spec",
  "verification",
];

/** Every subset of the panels a reader may have opened, as `activatedTabs` holds them. */
function everyActivation(): readonly Record<IssueTab, boolean>[] {
  const subsets: Record<IssueTab, boolean>[] = [];
  for (let mask = 0; mask < 1 << TABS.length; mask += 1) {
    const activated = {} as Record<IssueTab, boolean>;
    TABS.forEach((tab, index) => {
      activated[tab] = (mask & (1 << index)) !== 0;
    });
    subsets.push(activated);
  }
  return subsets;
}

test("the spec's panel draws it unless the address opens another document", () => {
  const activated = { ...Object.fromEntries(TABS.map((tab) => [tab, true])) } as Record<
    IssueTab,
    boolean
  >;
  const open = { activated, plan, selected: spec, spec, verification };
  expect(drawnDocuments({ ...open, otherDocument: false }).spec).toBe(spec);
  expect(drawnDocuments({ ...open, otherDocument: false }).selected).toBeUndefined();
  const elsewhere = drawnDocuments({ ...open, otherDocument: true, selected: notes });
  expect(elsewhere.spec).toBeUndefined();
  expect(elsewhere.selected).toBe(notes);
});

test("a panel the reader never opened draws nothing, and an artifact that is no document none", () => {
  const none = Object.fromEntries(TABS.map((tab) => [tab, false])) as Record<IssueTab, boolean>;
  const shut = drawnDocuments({
    activated: none,
    otherDocument: false,
    plan,
    selected: spec,
    spec,
    verification,
  });
  expect(drawnDocumentIds(shut).size).toBe(0);
  const blob = drawnDocuments({
    activated: { ...none, artifacts: true },
    otherDocument: true,
    plan,
    selected: picture,
    spec,
    verification,
  });
  expect(blob.selected).toBeUndefined();
});

test("a document a panel draws never also gets the page's own editor, at any address", () => {
  const artifacts = [spec, plan, verification, notes, picture];
  for (const activated of everyActivation()) {
    for (const otherDocument of [false, true]) {
      const selected = otherDocument ? notes : spec;
      const drawn = drawnDocuments({
        activated,
        otherDocument,
        plan,
        selected,
        spec,
        verification,
      });
      const ids = drawnDocumentIds(drawn);
      for (const editing of artifacts.map((held) => held.id)) {
        const edited = editedDocument(drawn, editing, artifacts);
        if (ids.has(editing)) {
          expect(edited, `${editing} is drawn by a panel and by the page as well`).toBeUndefined();
        } else {
          expect(edited?.id, `${editing} is edited in a document no panel draws`).toBe(editing);
        }
      }
      expect(editedDocument(drawn, undefined, artifacts)).toBeUndefined();
      // A row of a document this issue no longer holds gets no editor rather than an error.
      expect(editedDocument(drawn, "artifact-gone", artifacts)).toBeUndefined();
    }
  }
});
