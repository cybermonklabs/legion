import { beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { ApiError, api } from "../../api/client";
import type { GraphReferences, IssueReferences } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { buildIssuePath } from "../refs/routes";
import { NoTooltips } from "./NoTooltips";
import { OpenedDocument } from "./OpenedDocument";

/**
 * A document opened in place of the outline (part N's R2), drawn as the new issue page draws it:
 * inside `NoTooltips`, with the page's document reads answered and a fake room for its editor.
 */

answerTextReadsWithTheSeededText();

/** The document's own reads, and what references it and the issue, which nothing does here. */
beforeEach(() => {
  answerRequests(
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({
        linked_from: [],
        links: [],
        members: [],
        truncated: false,
      } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    ...documentSurfaceHandlers()
  );
});

/** The issue's spec, opened at its own address under `NoTooltips`. */
function renderOpened() {
  const client = createQueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const outline = buildIssuePath({ key: "CORE-1", kind: "issue" });
  return render(
    <MemoryRouter
      initialEntries={[buildIssuePath({ key: "CORE-1", kind: "artifact", slug: artifact.slug })]}
    >
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <NoTooltips>
              <OpenedDocument
                artifact={artifact}
                artifactRoute={{ key: "CORE-1", kind: "artifact", slug: artifact.slug }}
                highlightTerm={undefined}
                isClosed={false}
                issueKey="CORE-1"
                item={undefined}
                onShowDiff={() => {}}
                onToolbarChange={() => {}}
                onVersionChange={() => {}}
                outline={outline}
                showDiff={false}
              />
            </NoTooltips>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// R16 (decision 259): a document whose stored history cannot load offers a Rebuild. Pressed by
// keyboard, the rebuild lands, the rebuilt document reads, and the offer leaves the page with the
// button that held the focus, which hands it to nothing (decision 223). The opened document's
// region then puts the focus on its heading, the document's name, and never leaves it on the
// page's body.
test("a Rebuild that succeeds puts the focus on the opened document's heading", async () => {
  const user = userEvent.setup();
  const getArtifactText = spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, {
      code: "DOCUMENT_UNLOADABLE",
      error: "The stored history cannot load.",
    })
  );
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockResolvedValue({
    head: 1,
    removed_checkpoints: 0,
    removed_snapshots: 0,
    removed_updates: 3,
    source_version: 1,
    validation_error: "decode live document: unexpected end of input",
  });
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const view = renderOpened();
  try {
    const rebuild = await screen.findByRole("button", { name: "Rebuild from the latest version" });
    const heading = screen.getByRole("heading", { level: 2, name: artifact.name });
    getArtifactText.mockResolvedValue({
      markdown: "The live document",
      token: "sha256:live-document",
      version: 1,
    });
    rebuild.focus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(rebuildArtifact).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(rebuild.isConnected).toBe(false));
    await waitFor(() => expect(heading).toHaveFocus());
    expect(document.activeElement).not.toBe(document.body);
    expect(
      screen.queryByRole("button", { name: "Rebuild from the latest version" })
    ).not.toBeInTheDocument();
    expect(confirm).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
    getArtifactText.mockRestore();
  }
});
