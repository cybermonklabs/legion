import { afterEach, expect, spyOn, test } from "bun:test";
import type { LensInvoked } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
import { type RenderResult, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";

import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { buildDispatchReference } from "../../refs/routes";
import { KEY, READER_ACTOR } from "../decisions-filter/decisions-fixtures";
import { LENS_WORDS } from "../page-words";
import { treeOf } from "../slots/stage-fixtures";
import { treeNode } from "../tree-read/tree-fixtures";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import { answerSeededReads, drawPage, ON_STATEMENT, SERVES, seededNodes } from "./margin-fixtures";

/**
 * R7: each comment of a thread on text carries the Lens act of part W in Conversation and in
 * Graph, as its row would, and in no other view; a lens invoked from it is drawn in its thread,
 * last of that comment's lenses, with the focus on it. The pages are drawn by the page's own root
 * (`drawPage`), so the column, its cards and the address a pick lands on are the page's.
 */

/** The root of the thread the cases draw, and the address the Lens act is invoked on: the
 *  comment's own, as the tree read's node of a comment carries it. */
const ROOT = ON_STATEMENT.comment.id;
const COMMENT_ADDRESS = buildDispatchReference({ id: ROOT, key: KEY, kind: "comment" });

/** The lens the thread already holds, and the one a pick makes, by the ids their addresses carry
 *  in the tree read. */
const FIRST_LENS = "0c6f1d2e-1000-4a6b-9c1d-5e2f3a4b5c60";
const NEW_LENS = "0c6f1d2e-2000-4a6b-9c1d-5e2f3a4b5c60";

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** The card the column draws for the thread whose root is `root`, once it is drawn. */
async function cardOf(view: RenderResult, root: string): Promise<HTMLElement> {
  return waitFor(() => {
    const card = view.container.querySelector<HTMLElement>(`[data-margin-item="${root}"]`);
    if (card === null) throw new Error(`the page draws no card for the thread of ${root}`);
    return card;
  });
}

/** The Lens act inside `card`, or null. */
function lensActIn(card: HTMLElement): HTMLButtonElement | null {
  return card.querySelector<HTMLButtonElement>("[data-row-lens]");
}

// R7: the Lens act is the row's act of Conversation and Graph, where W draws it (`useLensAct`);
// every other view draws none, so a thread's comment carries none there.
test("a thread comment carries Lens in Conversation and Graph and not in Spec", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT] });
  const client = newClient();
  const seen: Record<string, string | null> = {};
  try {
    for (const preset of ["conversation", "graph", "spec"] as const) {
      const view = drawPage(client, preset);
      try {
        const card = await cardOf(view, ROOT);
        seen[preset] = lensActIn(card)?.textContent ?? null;
      } finally {
        view.finish();
      }
    }
  } finally {
    client.clear();
  }
  expect(seen, "the Lens act each view's card draws for the thread's comment").toEqual({
    conversation: LENS_WORDS.act,
    graph: LENS_WORDS.act,
    spec: null,
  });
});

/** A lens W's row draws, on the thread's comment, with the verdict `verdict`. */
function lensNode(id: string, verdict: string, at: string): TreeAnswerNode {
  return treeNode(id, "lens", ROOT, {
    attributes: { verdict },
    author: [READER_ACTOR],
    made_at: at,
  });
}

/** What the invocation of a lens of kind `kind` on the comment answers. */
function invoked(kind: string): LensInvoked {
  return {
    address: `dispatch://lens/${NEW_LENS}`,
    author: { id: "alice", kind: "person" },
    created_at: "2026-11-17T12:00:00Z",
    findings: [],
    id: NEW_LENS,
    kind,
    on: COMMENT_ADDRESS,
    state: "live",
    triggered_by: null,
    verdict: null,
    visibility: "project",
  };
}

// R7: a pick writes the lens on the comment's address, and the page lands on the lens the answer
// names (`?node=`), which is no row of the outline: the thread draws it, after the comment's other
// lenses, in the order the tree read gives, and the focus is on it.
test("a lens invoked from a thread stands last under its comment with the focus", async () => {
  const user = userEvent.setup();
  const reads = { threads: [ON_STATEMENT] };
  stubs = answerSeededReads(reads);
  const lenses: TreeAnswerNode[] = [lensNode(FIRST_LENS, "It holds.", "2026-11-17T10:00:00Z")];
  stubs.push(
    spyOn(api, "getIssueTree").mockImplementation(async () =>
      treeOf([...seededNodes(reads.threads), ...lenses], SERVES)
    )
  );
  const bodies: unknown[] = [];
  answerRequests(
    http.get("/api/v1/schema/blocks", () =>
      Response.json({
        lens_kinds: [
          {
            id: "pre-mortem",
            ref: "dispatch://lens_kind/pre-mortem",
            sources: null,
            words: "pre-mortem",
          },
        ],
        types: [],
        version: 1,
      })
    ),
    http.post(`/api/v1/issues/${KEY}/lenses`, async ({ request }) => {
      bodies.push(await request.json());
      lenses.push(lensNode(NEW_LENS, "It may not.", "2026-11-17T12:00:00Z"));
      return Response.json(invoked("pre-mortem"), { status: 201 });
    })
  );
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const card = await cardOf(view, ROOT);
    await waitFor(() =>
      expect(
        [...card.querySelectorAll("[data-thread-lens]")].map((row) =>
          row.getAttribute("data-thread-lens")
        ),
        "the thread draws its comment's lens"
      ).toEqual([FIRST_LENS])
    );
    const act = lensActIn(card);
    expect(act, "the comment carries no Lens act").not.toBeNull();
    await user.click(act as HTMLButtonElement);
    const kind = await waitFor(() => {
      const item = view.container.querySelector<HTMLElement>('[role="menu"] [role="menuitem"]');
      if (item === null) throw new Error("the menu lists no kind");
      return item;
    });
    await user.click(kind);
    await waitFor(() => expect(bodies, "the pick wrote no lens").toHaveLength(1));
    expect(bodies[0], "the write names the comment's address and the kind").toEqual({
      kind: "pre-mortem",
      on: COMMENT_ADDRESS,
    });
    await waitFor(() =>
      expect(
        [...card.querySelectorAll("[data-thread-lens]")].map((row) =>
          row.getAttribute("data-thread-lens")
        ),
        "the new lens does not stand last of the comment's lenses"
      ).toEqual([FIRST_LENS, NEW_LENS])
    );
    await waitFor(() =>
      expect(
        document.activeElement?.getAttribute("data-thread-lens"),
        "the focus is not on the new lens"
      ).toBe(NEW_LENS)
    );
  } finally {
    view.finish();
    client.clear();
  }
});
