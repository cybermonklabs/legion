import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen } from "@testing-library/react";
import { Suspense } from "react";
import { MemoryRouter } from "react-router-dom";

import { holdWithin } from "../../__tests__/content-drawn";
import { api } from "../../api/client";
import { issueQuery, whoAmIQuery } from "../../api/queries";
import { issue } from "../margin/margin-fixture";
import {
  contentDrawn,
  HOLD_LIMIT_MS,
  holdUntilContentDraws,
  whenContentDrawn,
} from "../shell/content-drawn";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { IssuePage } from "./IssuePage";
// The switch loads the new page as a lazy chunk. The first compile of that module is not what
// this row measures, so the test file loads the module before the row starts.
import "./IssueOutlinePage";

const ALICE_KEY = userPreferenceStorageKey("alice", "issue.page");

/**
 * How long the row lets the hold stay on after the page draws: a tenth of the hold's limit. The
 * switch ends the hold one frame after the page draws, which takes some milliseconds. If the
 * switch does not end it, the hold stays on until its limit. A tenth of the limit tells the two
 * apart on a busy machine, and a failing run stops after one second, not after ten.
 */
const AFTER_DRAW_MS = HOLD_LIMIT_MS / 10;

afterEach(async () => {
  // A hold one test left on would hold every test after it: end it, and wait for its frame.
  contentDrawn();
  await whenContentDrawn();
});

/** A client whose sign-in read holds alice and whose issue read holds CORE-1, and that never
 *  asks either again. */
function aliceOnCore1(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(issueQuery(issue.key).queryKey, issue);
  return client;
}

/** The reads the new page makes as it draws its tree (`useIssueTree.ts`), its header (the
 *  viewer's state, `useIssueDetail.ts`), Details' properties, which it draws for the life of
 *  the page (`slots/details.tsx`): the project list, which the repository's control takes its
 *  project's default repository from, and the project's architecture sources, and the outline's
 *  read mark, which reads the issue's newest event (`IssueReadMark.tsx`). Each is answered
 *  empty. The row is about when the hold ends, not about what the page holds, and a read nothing
 *  answered fails the file (`__tests__/setup.ts`). Each returns its spy, to restore. */
function answerPageReadsEmpty() {
  return [
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: issue.key,
      requirements: [],
      spec_artifact_id: "",
    }),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [],
      issue: issue.key,
      verification_artifact_id: "",
    }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
  ];
}

test("a landing on the new issue page ends the shell's hold on its extras when the page draws, not at the hold's limit", async () => {
  window.localStorage.setItem(ALICE_KEY, "outline");
  // `preloadRouteChunk` in `app.tsx` starts this hold from the address of an issue's Spec,
  // before React renders. The address does not tell which page the reader chose.
  holdUntilContentDraws();
  // The hold is on before the page renders, so the row cannot pass with no hold at all.
  expect(await holdWithin(0)).toBe("still on");
  const reads = answerPageReadsEmpty();
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={aliceOnCore1()}>
        <Suspense fallback={null}>
          <IssuePage />
        </Suspense>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const title = await screen.findByRole("heading", { name: issue.title });
    // The new page drew the title: today's page draws it too, but not in this root.
    expect(title.closest("[data-issue-page]")).toBeInTheDocument();
    // No key or pointer press goes to the window, and the hold's limit is far away, so only the
    // switch can end the hold here. The page's own reads may still be answering as it waits - on
    // a loaded machine they land after the title - so the wait is inside `act`, as every real
    // wait of a row with a page on screen is (`onRealTurns`).
    expect(await act(() => holdWithin(AFTER_DRAW_MS))).toBe("ended");
  } finally {
    view.unmount();
    for (const read of reads) read.mockRestore();
    window.localStorage.removeItem(ALICE_KEY);
  }
});
