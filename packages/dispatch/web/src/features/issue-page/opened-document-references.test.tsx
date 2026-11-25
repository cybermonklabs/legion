import { beforeEach, expect, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
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
import type { GraphReferences, IssueReferences } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { buildIssuePath } from "../refs/routes";
import { NoTooltips } from "./NoTooltips";
import { OpenedDocument } from "./OpenedDocument";
import { PROPERTY_LABELS } from "./page-structure";

/**
 * What references a document opened in place of the outline (part N's R2): the outline, which
 * holds the issue's own questions, comments and messages, is not drawn beside it, so the list
 * keeps what the issue's own comment cites of the document, as today's document page does.
 */

answerTextReadsWithTheSeededText();

const OWN_COMMENT = "11111111-1111-4111-8111-111111111111";
const OWN_WORDS = "CORE-1's own comment cites the spec.";

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
    // What references the spec: one comment of the spec's own issue, citing it.
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [
          {
            created_at: "2026-11-16T10:00:00Z",
            direction: "in",
            excerpt: { text: OWN_WORDS },
            kind: "mentions",
            node: graphNode({
              id: OWN_COMMENT,
              issue_key: "CORE-1",
              kind: "comment",
              project: "CORE",
              ref: `dispatch://CORE-1/comment/${OWN_COMMENT}`,
            }),
            source_seq: 7,
          },
        ],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    ...documentSurfaceHandlers()
  );
});

test("an opened document's Referenced by keeps the issue's own comment", async () => {
  const user = userEvent.setup();
  const client = createQueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const view = render(
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
                outline={buildIssuePath({ key: "CORE-1", kind: "issue" })}
                showDiff={false}
              />
            </NoTooltips>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const opened = screen.getByTestId("opened-artifact");
    // The list is folded under its control until the reader opens it (R2).
    expect(within(opened).queryByRole("region", { name: "Referenced by" })).toBeNull();
    await user.click(within(opened).getByRole("button", { name: PROPERTY_LABELS.referenced_by }));
    const list = within(opened).getByRole("region", { name: "Referenced by" });
    expect(
      await within(list).findByText(OWN_WORDS),
      "the opened document's list leaves out what the issue's own comment cites of it"
    ).toBeInTheDocument();
    expect(within(list).getByRole("link", { name: "Comment · CORE-1" })).toHaveAttribute(
      "href",
      `/issues/CORE-1/comments/${OWN_COMMENT}`
    );
  } finally {
    view.unmount();
    client.clear();
  }
});
