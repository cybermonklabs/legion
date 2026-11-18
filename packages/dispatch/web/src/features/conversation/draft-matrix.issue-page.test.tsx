/**
 * The draft matrix, issue page row: the docked composer and the phone thread composer as the
 * issue page draws them (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): "a move of the host's ... carries nothing: the text stays with the
 * address it was written for, and is there when the reader comes back to it", and "what it holds
 * when its host takes it off the page ... waits the same way for the composer drawn at that
 * address next." A refused or stopped send's note stays at its own address. Each cell writes a
 * draft in one state - typed and never sent, refused by Dispatch, or stopped by the reader -
 * makes one move of the issue page's, comes back, and asserts what the composer holds.
 *
 * Hosts: the issue page (`IssuePage` in `features/issue-page/IssuePage.tsx`, the route element
 * of `/issues/:key/*`), which draws today's page (`features/issue/IssuePage.tsx`) and in it the
 * Conversation tab (`ConversationTab`) with its docked composer and, on a phone, its thread
 * view's composer. The rows are the issue page's own moves the composer census found above
 * `ConversationTab`:
 *
 * | Host · move                                         | typed  | refused | stopped |
 * | --------------------------------------------------- | ------ | ------- | ------- |
 * | docked composer · issue left, then back (off)       | cell   | cell    | cell    |
 * | docked composer · another tab, then back (hidden)   | cell   | cell    | cell    |
 * | phone thread · issue left, then back (off)          | cell   | cell    | cell    |
 * | phone thread · another tab, then back (hidden)      | cell   | cell    | cell    |
 * | either · page reads fail or wait, then recover      | no (1) | no (1)  | no (1)  |
 * | either · the issue page choice moves to the outline | no (2) | no (2)  | no (2)  |
 * | either · the Conversation tab first drawn           | no (3) | no (3)  | no (3)  |
 * | either · the reader's issue state changes           | no (4) | no (4)  | no (4)  |
 * | either · an issue address that names no issue       | no (5) | no (5)  | no (5)  |
 *
 * (off) marks a move that takes the composer off the page: another issue's address gives the
 * page a new `route.key`, which draws `IssueDetail` afresh (`key={route.key}`), so the
 * Conversation and both composers unmount, and coming back draws them again. (hidden) marks one
 * that keeps the composer mounted but out of sight: another tab hides the Conversation's panel
 * (`hidden={activeTab !== "conversation"}`) and keeps it drawn, so coming back finds the same
 * composer. Both are checked on return alone, where the field holds the draft with its refusal or
 * stopped note. The phone thread's tab move is made through the address, as the browser's Back
 * and Forward make it, since the thread view covers the tab list.
 *
 * The moves the census found that these rows do not make:
 * 1. `IssueDetail` draws an error or a loading line in place of the tabs while a read of the
 *    issue, the reader's issue state or an item link's landing fails or is pending, which ends
 *    the Conversation's mount as leaving the issue does; the composer it draws again on recovery
 *    is the one the issue-left row draws again on return.
 * 2. `IssuePage` draws the outline page in place of today's when the reader's stored page choice
 *    says so (`useIssuePageChoice`), which ends the Conversation's mount as leaving the issue
 *    does; choosing today's page again draws it afresh, as the issue-left row's return does.
 * 3. `activatedTabs` only ever gains a tab: the Conversation is drawn the first time its tab is
 *    active and stays drawn, so this conditional starts the mount and never ends one.
 * 4. `state`, which `IssueDetail` hands the Conversation, is the reader's read mark and pins for
 *    the issue (`stateForIssue`), which only the unread boundary, the read mark's dwell and each
 *    card's pin read, so it moves no composer's address and draws or ends no composer.
 * 5. An address under `/issues/` that parses as no issue draws the not-found page in place of
 *    `IssueDetail` (`route === undefined`), so no composer is drawn there, and going there from
 *    an issue is leaving the issue, the issue-left row.
 *
 * The forms the composer census found: none in `IssuePage` or `IssueDetail`, which draw the page
 * the same at every width. The layout's forms under them are `ConversationTab`'s, which the docked
 * and phone thread files map, and the app shell's above them are the sign-in file's.
 *
 * `draft-matrix.issue-page.coverage.ts` holds each census item this file answers for to a test
 * above or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import { type MutationKey, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import {
  alice,
  BODY,
  commentA,
  commentB,
  expectedBack,
  HELD,
  type Held,
  listTurn,
  newQueryClient,
  noteIn,
  REFUSAL,
  REFUSED_NOTE,
} from "../../__tests__/draft-matrix-fixture";
import { answerRequests } from "../../__tests__/network";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { IssuePage } from "../issue-page/IssuePage";
import { MarginProvider } from "../margin/margin-context";
import { issue, specArtifact, stubMatchMedia } from "../margin/margin-fixture";
import { buildIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import type { SentRequest } from "./send-request";

/** The docked composer's send name (`ConversationTab`'s `sendKey`), and the phone thread
 *  composer's (`usePhoneThread`'s `composerKey`). */
const DOCKED_KEY: MutationKey = ["conversation-composer", "CORE-1"];
const THREAD_KEY: MutationKey = ["conversation-composer", "CORE-1", "phone-thread"];

/** Another issue, whose page the reader goes to and comes back from. */
const otherIssue: IssueDetails = {
  ...issue,
  artifacts: [{ ...specArtifact, id: "artifact-2", issue_key: "CORE-2", ref_key: "CORE-2/spec" }],
  key: "CORE-2",
  number: 2,
  primary_artifact_id: "artifact-2",
  title: "Second issue",
};

/** A send of `BODY` on CORE-1: a comment on the issue from the docked composer, or the thread
 *  composer's reply to Comment A, the comment the phone cells answer. */
function request(replyToA: boolean): SentRequest {
  return sentRequest(
    BODY,
    replyToA ? { actor: alice, excerpt: "Comment A", id: commentA.id, parentKind: "comment" } : null
  );
}

const conversationOf = (key: string) => buildIssuePath({ key, kind: "conversation" });

/** The reader's ways about: to another issue and back, and to another tab of CORE-1 and back,
 *  each as the address it writes. */
function Travel(): ReactNode {
  const navigate = useNavigate();
  return (
    <>
      <button onClick={() => navigate(conversationOf(otherIssue.key))} type="button">
        Open the other issue
      </button>
      <button onClick={() => navigate(conversationOf(issue.key))} type="button">
        Back to the first issue
      </button>
      <button
        onClick={() => navigate(buildIssuePath({ key: issue.key, kind: "activity" }))}
        type="button"
      >
        Open the Activity tab
      </button>
    </>
  );
}

interface Page {
  readonly createComment: Mock<typeof api.createComment>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/**
 * CORE-1's issue page on its Conversation tab, at a phone width or above it, over Comment A and
 * Comment B. Every comment the page writes is refused, so a send pressed here comes back refused;
 * `stopped`, when given, is sent through the held-send store under its name before the page is
 * drawn and stopped once late, as the reader's Stop trying does, so the page arrives showing it.
 */
async function openIssuePage(
  phone: boolean,
  stopped?: { readonly key: MutationKey; readonly request: SentRequest }
): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const state = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };
  const reads = [
    spyOn(api, "getIssue").mockImplementation(async (key) =>
      key === otherIssue.key ? otherIssue : issue
    ),
    spyOn(api, "getMyState").mockResolvedValue({ "CORE-1": state, "CORE-2": state }),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listComments").mockImplementation(async (key) =>
      key === issue.key ? [commentA, commentB] : []
    ),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listHandoffs").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
  ];
  // The header's own reads, each answered with the API's empty shape.
  answerRequests(
    http.get("/api/v1/settings/repo-projects", () => Response.json([])),
    http.get("/api/v1/projects/:project/architecture-sources", () => Response.json([]))
  );
  const createComment = spyOn(api, "createComment");
  if (stopped !== undefined) {
    await stopSend(queryClient, stopped.key, stopped.request, [createComment]);
  }
  createComment.mockRejectedValue(REFUSAL);
  const restoreViewport = stubMatchMedia(phone);
  const runtime = fakeDocumentRuntime();
  const view = render(
    <MemoryRouter
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      initialEntries={[conversationOf(issue.key)]}
    >
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <DocumentRuntime.Provider value={runtime.runtime}>
            <MarginProvider>
              <Travel />
              <Routes>
                <Route element={<IssuePage />} path="/issues/:key/*" />
              </Routes>
            </MarginProvider>
          </DocumentRuntime.Provider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  await listed();
  return {
    createComment,
    restore: () => {
      view.unmount();
      restoreViewport();
      createComment.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

/** Waits for CORE-1's Conversation to list both comments. */
async function listed(): Promise<void> {
  await waitFor(() =>
    expect(document.querySelector('[data-turn="comment:comment-b"]')).not.toBe(null)
  );
}

/** Goes to CORE-2's Conversation, which lists nothing, and back to CORE-1's. */
async function leaveTheIssueAndComeBack(user: UserEvent): Promise<void> {
  await user.click(screen.getByRole("button", { name: "Open the other issue" }));
  await waitFor(() => expect(document.querySelector('[data-turn="comment:comment-a"]')).toBe(null));
  await screen.findByText("No conversations yet.");
  await user.click(screen.getByRole("button", { name: "Back to the first issue" }));
  await listed();
}

/** Goes to CORE-1's Activity tab, which hides the Conversation's panel, and back. */
async function openAnotherTabAndComeBack(user: UserEvent): Promise<void> {
  await user.click(screen.getByRole("button", { name: "Open the Activity tab" }));
  await waitFor(() =>
    expect(document.getElementById("issue-conversation-panel")?.hidden).toBe(true)
  );
  await user.click(screen.getByRole("button", { name: "Back to the first issue" }));
  await waitFor(() =>
    expect(document.getElementById("issue-conversation-panel")?.hidden).toBe(false)
  );
}

// ---------------------------------------------------------------------------------------------
// The docked composer, above the phone layout.

const docked = () => screen.getByRole("form", { name: "Comment composer" });
const dockedField = () =>
  within(docked()).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });

/** Puts `BODY` in the docked composer as a comment on CORE-1, in the state `held` names; a
 *  stopped comment is the one `openIssuePage` seeded, which the composer mounted with. */
async function holdInDocked(page: Page, held: Held): Promise<void> {
  if (held === "stopped") {
    await waitFor(() => expect(noteIn(docked())).toBe("stopped"));
    return;
  }
  await page.user.click(dockedField());
  await page.user.paste(BODY);
  if (held === "refused") {
    await page.user.click(within(docked()).getByRole("button", { name: "Send" }));
    await within(docked()).findByText(REFUSED_NOTE);
  }
}

const stoppedDocked = (held: Held) =>
  held === "stopped" ? { key: DOCKED_KEY, request: request(false) } : undefined;

const DOCKED_MOVES = [
  { move: leaveTheIssueAndComeBack, name: "issue left, then back" },
  { move: openAnotherTabAndComeBack, name: "another tab, then back" },
] as const;

for (const { move, name } of DOCKED_MOVES) {
  for (const held of HELD) {
    test(`docked composer · ${name} · ${held}`, async () => {
      const page = await openIssuePage(false, stoppedDocked(held));
      try {
        await holdInDocked(page, held);
        await move(page.user);
        expect({ note: noteIn(docked()), text: dockedField().value }).toEqual(expectedBack(held));
      } finally {
        page.restore();
      }
    });
  }
}

// ---------------------------------------------------------------------------------------------
// The phone thread composer.

/** Presses Comment A's Reply in the list and answers the thread view it opens. */
async function replyToA(user: UserEvent): Promise<HTMLElement> {
  await user.click(within(listTurn(commentA.id)).getByRole("button", { name: "Reply" }));
  return screen.findByRole("dialog", { name: "Thread" });
}

const threadField = (dialog: HTMLElement) =>
  within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });

/** Opens Comment A's thread with its Reply and puts `BODY` in the thread composer, in the state
 *  `held` names; a stopped reply is the one `openIssuePage` seeded. */
async function holdInThreadA(page: Page, held: Held): Promise<void> {
  const dialog = await replyToA(page.user);
  if (held === "stopped") {
    expect(noteIn(dialog)).toBe("stopped");
    return;
  }
  await page.user.click(threadField(dialog));
  await page.user.paste(BODY);
  if (held === "refused") {
    await page.user.click(within(dialog).getByRole("button", { name: "Send" }));
    await within(dialog).findByText(REFUSED_NOTE);
  }
}

const stoppedThread = (held: Held) =>
  held === "stopped" ? { key: THREAD_KEY, request: request(true) } : undefined;

/** Comment A's thread view as the reader finds it on return: still open where the page kept it,
 *  or opened again with Comment A's Reply. */
async function threadAOnReturn(user: UserEvent): Promise<HTMLElement> {
  return screen.queryByRole("dialog", { name: "Thread" }) ?? replyToA(user);
}

for (const { move, name } of DOCKED_MOVES) {
  for (const held of HELD) {
    test(`phone thread · ${name} · ${held}`, async () => {
      const page = await openIssuePage(true, stoppedThread(held));
      try {
        await holdInThreadA(page, held);
        await move(page.user);
        const dialog = await threadAOnReturn(page.user);
        expect({ note: noteIn(dialog), text: threadField(dialog).value }).toEqual(
          expectedBack(held)
        );
      } finally {
        page.restore();
      }
    });
  }
}
