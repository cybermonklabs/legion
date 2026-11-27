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
import { rowSessionId } from "./test";
import type { SignInLogin } from "./users";

/** How many sessions k11 pins. */
const COST_RIGS = 8;

/** How many turns each rig's replay carries: the publisher's own cap, so a view that drew them
 *  all drew the most a session can ever answer with. */
export const COST_RIG_TURNS = AGENT_STREAM_LIMITS.historyMessages;

/** The `index`th live session for the rail to pin, under the running row's own id for it
 *  (`rowSessionId`, e2e/test.ts). `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function costRig(index: number): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/cost-${index + 1}`,
    last_seen: Date.now() - 5_000,
    machine_id: "cost-host",
    roles: [],
    session_id: rowSessionId(`cost${index + 1}`),
    title: `Cost ${index + 1}`,
  };
}

/**
 * Eight live sessions, each pinned by `login` and each answering the next viewer's replay request
 * with a conversation at the stream's limit, and answers their ids in the order they were pinned.
 * The pins go one at a time, since the rail's order is the order they were made in, and every
 * view a row opens is handed the same weight of transcript.
 *
 * Each id is the running row's own (`rowSessionId`, e2e/test.ts), a UUID, since Dispatch's agent
 * routes refuse anything else: the live view shows the messages Dispatch keeps for a session,
 * by its id for every person at once, beside the turns its replay holds. `login` is the row's own
 * person (`rowPerson`) for the same reason on the person's side: the rail lists every pin they
 * hold.
 */
export async function seedPinnedRigsAtLimit(login: SignInLogin): Promise<string[]> {
  const sessions = Array.from({ length: COST_RIGS }, (_, index) => costRig(index));
  const ids = sessions.map((session) => session.session_id);
  await setLiveSessions(sessions);
  await seedAgentStreamConversations(ids);
  for (const sessionID of ids) {
    await putAgentState(sessionID, { pinned: true }, { login });
  }
  return ids;
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
