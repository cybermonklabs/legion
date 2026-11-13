import { expect, test } from "bun:test";

import type { Artifact } from "../../api/types";
import type { IssueTab } from "../refs/routes";
import { type DrawnDocuments, drawnDocuments, editedDocument } from "./drawn-documents";

/**
 * One document has one editor and never a second (`document-stage.tsx`). The page and its
 * panels both read `drawnDocuments`, so the rows below hold that one answer to the rule: one
 * document is named by one field of it, and a document a panel draws never also gets the
 * page's own editor for an edited row, whichever address is open and whichever panels have
 * been opened.
 */

/** How many of the page's four drawing sites draw the document `id`. Each field a panel reads
 *  mounts one editor, so two sites are two editors - two sockets - on one document.
 *  It counts the fields themselves: `drawn-documents.ts`'s own id set is what `editedDocument`
 *  reads, so a test that asked it would be asking the implementation its own question, and a
 *  set holds one entry for a document two panels drew. */
function drawingSites(drawn: DrawnDocuments, id: string): number {
  return [drawn.plan, drawn.selected, drawn.spec, drawn.verification].filter(
    (document) => document?.id === id
  ).length;
}

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
  expect(shut).toEqual({
    plan: undefined,
    selected: undefined,
    spec: undefined,
    verification: undefined,
  });
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

// The defect the count above catches: with every panel opened and `/artifacts/plan-md` the
// address, the plan was named by `plan` and by `selected` both, so the Plan panel and the
// address's own panel each mounted an editor on it - two sockets on one document. The
// address's own document wins, and the preset's panel yields (`drawn-documents.ts`).
test("a document the address opens is drawn by that one panel and by no preset's as well", () => {
  const everyTab = Object.fromEntries(TABS.map((tab) => [tab, true])) as Record<IssueTab, boolean>;
  const open = { activated: everyTab, otherDocument: true, plan, spec, verification };

  const openPlan = drawnDocuments({ ...open, selected: plan });
  expect(openPlan.selected).toBe(plan);
  expect(openPlan.plan).toBeUndefined();
  expect(drawingSites(openPlan, "plan")).toBe(1);

  // The Verification document is an artifact of the issue too, and the same rule holds for it.
  const openChecks = drawnDocuments({ ...open, selected: verification });
  expect(openChecks.selected).toBe(verification);
  expect(openChecks.verification).toBeUndefined();
  expect(drawingSites(openChecks, "verification")).toBe(1);

  // A row of that document still takes the one editor the address's panel draws, rather than
  // getting the page's own.
  expect(editedDocument(openPlan, "plan", [spec, plan, verification])).toBeUndefined();
});

test("one document is named by one field and gets one editor, at any address", () => {
  const artifacts = [spec, plan, verification, notes, picture];
  // Every address the page distinguishes: its own and `/spec`, and an `/artifacts/<slug>` on
  // each kind of artifact it can name - a document of its own, the two a preset keeps open, an
  // artifact that is no document, and one that has not loaded yet.
  const addresses = [
    { otherDocument: false, selected: spec },
    { otherDocument: true, selected: notes },
    { otherDocument: true, selected: plan },
    { otherDocument: true, selected: verification },
    { otherDocument: true, selected: picture },
    { otherDocument: true, selected: undefined },
  ];
  for (const activated of everyActivation()) {
    for (const address of addresses) {
      const drawn = drawnDocuments({ activated, ...address, plan, spec, verification });
      for (const document of artifacts) {
        expect(
          drawingSites(drawn, document.id),
          `${document.id} is drawn by two panels at ${address.selected?.id ?? "no document"}`
        ).toBeLessThanOrEqual(1);
      }
      for (const editing of artifacts.map((held) => held.id)) {
        const edited = editedDocument(drawn, editing, artifacts);
        if (drawingSites(drawn, editing) > 0) {
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
