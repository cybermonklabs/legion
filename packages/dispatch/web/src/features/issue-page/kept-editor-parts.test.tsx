import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import * as Y from "yjs";

import { issueTreeKey } from "../../api/queries";
import type { Comment } from "../../api/types";
import { MarginProvider, useMargin } from "../margin/margin-context";
import { comment, issue, specArtifact } from "../margin/margin-fixture";
import { useMarginItems } from "../margin/useMarginItems";
import type { KeptBind } from "./block-editor";
import { anchoredCount, KeptBlockReferences } from "./kept-editor-parts";

function commentOn(id: string, blockId: string): Comment {
  if (comment.anchor === null) throw new Error("Fixture has no document anchor");
  return { ...comment, id, anchor: { ...comment.anchor, mark_id: id, block_id: blockId } };
}

function References({ bind, client }: { bind: KeptBind; client: QueryClient }) {
  const { blockFilterId, blockFilterGroup, filterToBlocks } = useMargin();
  const { threads } = useMarginItems(
    { key: issue.key, kind: "issue" },
    "comments",
    specArtifact,
    new Map(),
    new Map(),
    blockFilterId,
    { blockFilterIds: blockFilterGroup?.ids }
  );
  return (
    <>
      <KeptBlockReferences
        bind={bind}
        reference={anchoredCount(client, issue.key, bind)}
        onReferencesPress={(_, blockId, blocks) => filterToBlocks(blockId, blocks)}
      />
      <ul aria-label="Margin comment cards">
        {threads.map((thread) => (
          <li key={thread.key}>{thread.key}</li>
        ))}
      </ul>
    </>
  );
}

test("the bound block's references button counts and shows its nested comment cards", async () => {
  const doc = new Y.Doc();
  const element = new Y.XmlElement("requirement");
  const inner = new Y.XmlElement("paragraph");
  doc.getXmlFragment("spec").insert(0, [element]);
  element.setAttribute("blockId", "requirement");
  element.insert(0, [inner]);
  inner.setAttribute("blockId", "paragraph");
  const bind = { artifactId: specArtifact.id, box: document.createElement("div"), element };
  const comments = [
    commentOn("on requirement", "requirement"),
    commentOn("on paragraph", "paragraph"),
    commentOn("on another block", "other"),
  ];
  const client = new QueryClient({
    defaultOptions: { queries: { staleTime: Infinity, retry: false } },
  });
  client.setQueryData(issueTreeKey(issue.key), { nodes: comments });
  client.setQueryData(["comments", issue.key], comments);
  client.setQueryData(["inbox"], []);
  client.setQueryData(["asks", issue.key], []);
  client.setQueryData(["user-state"], {});
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>
          <References bind={bind} client={client} />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const button = view.getByRole("button", { name: "2 references on block" });
    expect(button.getAttribute("data-block-references")).toBe("requirement");
    await userEvent.setup().click(button);
    await waitFor(() => {
      const cards = view.getAllByRole("listitem");
      expect(cards).toHaveLength(Number(button.textContent));
      expect(cards.map((card) => card.textContent).sort()).toEqual([
        "on paragraph",
        "on requirement",
      ]);
    });
  } finally {
    view.unmount();
    client.clear();
    doc.destroy();
  }
});
