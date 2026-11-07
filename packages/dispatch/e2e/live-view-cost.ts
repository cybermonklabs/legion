// The rig both halves of k11 run on (INFRA-812, R2; the Verification document's k11,
// dispatch://INFRA-812/artifact/verification-md): eight agents one person has pinned, each
// seeded with a conversation at the stream's limit, which is the heaviest transcript a live
// view can ever be handed. The untimed half of the check - how many agent subscriptions the
// socket holds, how many conversations are drawn, the page's heap, and the negative control
// with the sleep rule off - is `agent-view-cost.e2e.ts`, in `chromium`. The one timed claim -
// a sleeping view at the stream's worst case comes back inside 1.2 seconds - is a row of
// `agent-workspace-timing.e2e.ts`, in `chromium-alone` (D14), and both halves seed from here so
// the figures each reads are of the same rig.
import { AGENT_STREAM_LIMITS } from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { putAgentState, seedAgentStreamConversations } from "./api";
import { frontLiveView } from "./live-views";

/** The eight sessions k11 pins, by id. Each is a UUID: Dispatch's agent routes refuse anything
 *  else, so a session named otherwise would list and have no live view at all. */
export const COST_RIG_IDS = [
  "01a0e812-c057-7000-8000-000000000001",
  "01a0e812-c057-7000-8000-000000000002",
  "01a0e812-c057-7000-8000-000000000003",
  "01a0e812-c057-7000-8000-000000000004",
  "01a0e812-c057-7000-8000-000000000005",
  "01a0e812-c057-7000-8000-000000000006",
  "01a0e812-c057-7000-8000-000000000007",
  "01a0e812-c057-7000-8000-000000000008",
];

/** How many turns each rig's replay carries: the publisher's own cap, so a view that drew them
 *  all drew the most a session can ever answer with. */
export const COST_RIG_TURNS = AGENT_STREAM_LIMITS.historyMessages;

/** A live session for the rail to pin. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function costRig(index: number): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/cost-${index + 1}`,
    last_seen: Date.now() - 5_000,
    machine_id: "cost-host",
    roles: [],
    session_id: COST_RIG_IDS[index],
    title: `Cost ${index + 1}`,
  };
}

/**
 * Eight live sessions, each pinned and each answering the next viewer's replay request with a
 * conversation at the stream's limit. The pins go one at a time, since the rail's order is the
 * order they were made in, and every view a row opens is handed the same weight of transcript.
 */
export async function seedPinnedRigsAtLimit(): Promise<void> {
  await setLiveSessions(COST_RIG_IDS.map((_, index) => costRig(index)));
  await seedAgentStreamConversations(COST_RIG_IDS);
  for (const sessionID of COST_RIG_IDS) {
    await putAgentState(sessionID, { pinned: true });
  }
}

/** The turns the view in front has drawn. Every rig answers with the same prose, so a row reads
 *  which session's conversation is on the page from the view's own id, never from its text. */
export function frontTurns(page: Page): Locator {
  return frontLiveView(page).getByTestId("agent-thread").locator("[data-message-id]");
}

/** Waits until `sessionID`'s view is the one in front with its whole conversation drawn: the
 *  transcript at the stream's limit, rebuilt from the session's replay. */
export async function expectConversationInFront(page: Page, sessionID: string): Promise<void> {
  await expect(frontLiveView(page)).toHaveAttribute("data-session-id", sessionID);
  await expect(frontTurns(page)).toHaveCount(COST_RIG_TURNS);
}
