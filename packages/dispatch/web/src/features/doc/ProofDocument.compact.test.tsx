import { beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { answerRequests } from "../../__tests__/network";
import {
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
  layOutAt,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import { MarginProvider } from "../margin/margin-context";
import { DESKTOP_WIDTH, PHONE_WIDTH } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ProofDocument } from "./ProofDocument";
import { DocumentRuntime } from "./runtime";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

beforeEach(() => {
  answerRequests(...documentSurfaceHandlers());
});

// The compact layout is the shell's own query (`COMPACT_VIEWPORT_QUERY`), which the viewport
// answers: the editor publishes no presence where it holds, and the page's does where it does
// not. A unit test's DOM lays nothing out, so a read of the page's width finds none at all.
for (const [layout, width] of [
  ["compact", PHONE_WIDTH],
  ["desktop", DESKTOP_WIDTH],
] as const) {
  test(`ProofDocument keeps transport active, and publishes presence only outside the compact layout: the ${layout} layout`, async () => {
    const restoreViewport = layOutAt(width);
    const runtime = fakeDocumentRuntime();
    const queryClient = createQueryClient();
    const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "",
      token: "sha256:empty-spec",
      version: null,
    });
    const view = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <DocumentRuntime.Provider value={runtime.runtime}>
            <MarginProvider>
              <ProofDocument
                artifact={artifact}
                highlight={undefined}
                isClosed={false}
                owner={{ key: "CORE-1", kind: "issue" }}
                onVersionChange={() => {}}
                showDiff={false}
                user={{ event_head: "0", kind: "user", login: "alice" }}
              />
            </MarginProvider>
          </DocumentRuntime.Provider>
        </QueryClientProvider>
      </MemoryRouter>
    );

    try {
      await waitFor(() => expect(runtime.connections).toHaveLength(1));
      act(() => runtime.sync());
      await waitFor(() => expect(runtime.editors).toHaveLength(1));
      expect(runtime.editors[0]?.options.awareness).toBe(
        layout === "compact" ? null : runtime.connections[0]?.awareness
      );
    } finally {
      view.unmount();
      getArtifactText.mockRestore();
      restoreViewport();
    }
  });
}
