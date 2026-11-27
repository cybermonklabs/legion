import { afterEach, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { type ReactNode, useEffect } from "react";
import { Link, MemoryRouter, useLocation } from "react-router-dom";

import { answerRequests } from "../../__tests__/network";
import { documentSurfaceHandlers } from "../../__tests__/proof-document";
import { issueQuery } from "../../api/queries";
import type { IssueCheckResults, IssueCoverage, ReferenceTargets } from "../../api/types";
import { afterTheFirstBatch, theFirstBatchIsAwaited } from "../doc/first-batch";
import { issue } from "../margin/margin-fixture";
import { IssueOutlinePage } from "./IssueOutlinePage";

// `issue` is CORE-1 with a primary spec, so its bare address is one that opens a document: the
// address the shell holds the page's other reads at and the server draws the spec's blocks for.
const SPEC_ADDRESS = `/issues/${issue.key}`;

/** The page's own reads (`useIssueTree`), made once the hold ends, which the rows do not read. */
function answerThePageReads(): void {
  answerRequests(
    http.get("/api/v1/issues/:key/coverage", ({ params }) =>
      Response.json({
        gaps: [],
        has_plan: false,
        issue: String(params.key),
        requirements: [],
        spec_artifact_id: "",
      } satisfies IssueCoverage)
    ),
    http.get("/api/v1/issues/:key/results", ({ params }) =>
      Response.json({
        checks: [],
        issue: String(params.key),
        verification_artifact_id: "",
      } satisfies IssueCheckResults)
    ),
    http.get("/api/v1/issues/:key/comments", () => Response.json([])),
    http.get("/api/v1/issues/:key/messages", () => Response.json([])),
    // The issue's head, which the end of the outline reads to mark the issue read
    // (`IssueReadMark.tsx`).
    http.get("/api/v1/issues/:key/events", () => Response.json([])),
    http.get("/api/v1/reference-targets", () =>
      Response.json({ targets: [] } satisfies ReferenceTargets)
    ),
    // The viewer's issue states, which the header's pin reads (`api.getMyState`).
    http.get("/api/v1/me/state", () => Response.json({})),
    // The project's architecture sources, which Details' components line reads.
    http.get("/api/v1/projects/:project/architecture-sources", () => Response.json([])),
    // The documents' blocks, the issue's asks and its decisions.
    ...documentSurfaceHandlers()
  );
}

beforeEach(answerThePageReads);

afterEach(() => {
  cleanup();
  theFirstBatchIsAwaited(false);
  document.body.innerHTML = "";
});

/** A client that holds CORE-1 and never asks for it again. */
function issueHeld(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(issueQuery(issue.key).queryKey, issue);
  return client;
}

/** The page the server sends for `address`: the build's `#first-screen` element after `#root`,
 *  with the document's opening blocks drawn into it (`features/doc/first-screen.test.tsx`). */
function pageCarryingADraw(address: string): void {
  document.body.innerHTML =
    `<div id="root"></div><div id="first-screen">` +
    `<div data-first-screen-address="${address}"><p data-block-id="b-1">Served</p></div></div>`;
}

/** What the shell's `OpenDocumentEarly` does at an address that opens a document: it arms the
 *  hold, in an effect that stands before the routes and so runs before the page's own. */
function ArmsTheHoldAtEachAddress(): null {
  const { pathname } = useLocation();
  useEffect(() => {
    if (pathname === SPEC_ADDRESS) theFirstBatchIsAwaited(true);
  }, [pathname]);
  return null;
}

function Page({ at }: { at: string }): ReactNode {
  return (
    <QueryClientProvider client={issueHeld()}>
      <MemoryRouter initialEntries={[at]}>
        <ArmsTheHoldAtEachAddress />
        <Link to={SPEC_ADDRESS}>Spec</Link>
        <IssueOutlinePage />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

test("at an address that opens the issue's spec, the new page ends the hold and takes the server's draw out", async () => {
  pageCarryingADraw(SPEC_ADDRESS);
  theFirstBatchIsAwaited(true);

  render(<Page at={SPEC_ADDRESS} />);

  expect(await screen.findByRole("heading", { level: 1, name: issue.title })).toBeDefined();
  // No editor mounts on this page, so nothing else would end the hold before `LONGEST_HOLD_MS`.
  expect(afterTheFirstBatch()).toBe(true);
  expect(document.getElementById("first-screen")).toBeNull();
});

test("the hold the shell arms again on a move inside the new page is ended at the address it moves to", async () => {
  const user = userEvent.setup();
  render(<Page at={`${SPEC_ADDRESS}/conversation`} />);
  await screen.findByRole("heading", { level: 1, name: issue.title });
  expect(afterTheFirstBatch()).toBe(true);

  await user.click(screen.getByRole("link", { name: "Spec" }));

  expect(afterTheFirstBatch()).toBe(true);
});
