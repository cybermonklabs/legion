/** The one agent component every surface draws, and the three sessions the rows are read over.
 *  Shared by `agent-name.e2e.ts` and the rows of that family which run against a recording
 *  (`agent-name.stub.e2e.ts`). */
import { expect, type Locator, type Page } from "@playwright/test";

import { type FakeSession, setLiveSessions, setSessionLive } from "./agents";
import {
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  mintAgentToken,
  patchIssue,
} from "./api";
import { marginCard } from "./editor";
// One agent component draws every agent the app shows (dispatch://INFRA-440): its current name,
// whether it is running, and a link to its live conversation. These rows hold it to that in the
// four places the issue names - "Claimed by", a Conversation turn, an Inbox ask card and a margin
// comment - for a running agent and one that is not running, at desktop and phone width.

// Ids that differ in their first eight characters, so their short ids differ too.
export const runId = "e2e-run-agent";
export const stopId = "e2e-stop-agent";
export const untitledId = "e2e-untitled";

export const runStamped = "Running agent (stamped)";
export const runCurrent = "Running agent (current)";
export const stopStamped = "Stopped agent (stamped)";
export const untitledShort = "session:e2e-unti…";
export const stopShort = "session:e2e-stop…";

export function writer(id: string, title?: string) {
  return {
    actor: {
      id,
      kind: "session" as const,
      origin: title === undefined ? { tmux: "dev:1.1" } : { session_title: title, tmux: "dev:1.1" },
    },
    as: "agent" as const,
  };
}

export const liveSessions: FakeSession[] = [
  { session_id: runId, title: runCurrent },
  { session_id: stopId, title: stopStamped },
  { session_id: untitledId, title: "" },
];

export interface Seeded {
  runIssue: string;
  stopIssue: string;
  runAsk: string;
  stopAsk: string;
  runMessage: string;
  stopMessage: string;
  untitledMessage: string;
  runComment: string;
  stopComment: string;
}

/** Real session writes from three sessions while all three are listed, then two of them leave
 *  the listener's list before any page loads. The running agent's claim is written with a
 *  personal token, so its label carries the owner suffix. The writes no row reads in order - the
 *  two claims, the three messages, the two asks - go together; the two margin comments stay in
 *  order behind them, since each anchors into the same spec. */
export async function seed(): Promise<Seeded> {
  await createProject({ key: "AGT", name: "Agents" });
  const runIssue = await createIssue({
    project: "AGT",
    spec: "The running agent comments here. The stopped agent comments there.",
    title: "Held by a running agent",
  });
  const stopIssue = await createIssue({ project: "AGT", title: "Held by a stopped agent" });
  await Promise.all([
    patchIssue(runIssue.key, { status: "todo" }),
    patchIssue(stopIssue.key, { status: "todo" }),
  ]);
  await setLiveSessions(liveSessions);
  const token = await mintAgentToken("agent-name-e2e", "alice");
  const [, , runMessage, stopMessage, untitledMessage, runAsk, stopAsk] = await Promise.all([
    claimIssue(runIssue.key, { ...writer(runId, runStamped), token }),
    claimIssue(stopIssue.key, writer(stopId, stopStamped)),
    createMessage(
      runIssue.key,
      { body: "Message from the running agent" },
      writer(runId, runStamped)
    ),
    createMessage(
      runIssue.key,
      { body: "Message from the stopped agent" },
      writer(stopId, stopStamped)
    ),
    createMessage(runIssue.key, { body: "Message from the untitled agent" }, writer(untitledId)),
    createAsk(
      runIssue.key,
      { question: "Question from the running agent" },
      writer(runId, runStamped)
    ),
    createAsk(
      runIssue.key,
      { question: "Question from the stopped agent" },
      writer(stopId, stopStamped)
    ),
  ]);
  const runComment = await createComment(
    runIssue.key,
    { anchor: { artifact: "spec", quote: "comments here" }, body: "Margin from the running agent" },
    writer(runId, runStamped)
  );
  const stopComment = await createComment(
    runIssue.key,
    {
      anchor: { artifact: "spec", quote: "comments there" },
      body: "Margin from the stopped agent",
    },
    writer(stopId, stopStamped)
  );
  await setSessionLive(stopId, false);
  await setSessionLive(untitledId, false);
  return {
    runAsk: runAsk.id,
    runComment: runComment.id,
    runIssue: runIssue.key,
    runMessage: runMessage.id,
    stopAsk: stopAsk.id,
    stopComment: stopComment.id,
    stopIssue: stopIssue.key,
    stopMessage: stopMessage.id,
    untitledMessage: untitledMessage.id,
  };
}

export function livePath(id: string): string {
  return `/agents/${encodeURIComponent(id)}/live`;
}

export function agentIn(scope: Locator, id: string): Locator {
  return scope.locator(`[data-agent-name="${id}"]`).first();
}

/** The component in `scope` for `id`: the name it shows, its state mark, and its live link. */
export async function expectAgent(
  scope: Locator,
  id: string,
  shown: { name: string; hidden?: string; running: boolean; link?: "name" | "beside" }
): Promise<void> {
  const agent = agentIn(scope, id);
  await expect(agent).toBeVisible();
  await expect(agent).toContainText(shown.name);
  if (shown.hidden !== undefined) {
    await expect(agent).not.toContainText(shown.hidden);
  }
  await expect(agent).toHaveAttribute(
    "data-agent-state",
    shown.running ? "running" : "not-running"
  );
  await expect(
    agent.getByRole("img", { exact: true, name: shown.running ? "Running" : "Not running" })
  ).toBeVisible();
  await expect(
    agent.getByRole("img", { exact: true, name: shown.running ? "Not running" : "Running" })
  ).toHaveCount(0);
  await expect(agent.locator(`a[href="${livePath(id)}"]`)).toHaveCount(1);
}

export function messageTurn(page: Page, id: string): Locator {
  return page.locator(`[data-turn="message:${id}"]`);
}

export async function openMarginThread(page: Page, commentId: string): Promise<Locator> {
  const card = marginCard(page, commentId);
  await card.locator('button[aria-expanded="false"]').first().click();
  return card;
}
