import { expect, test } from "@playwright/test";

import { type FakeSession, setLiveSessions, setSessionLive } from "./agents";
import { createAgentMessage, replyToMessageDelivery } from "./api";
import { inboxRow } from "./inbox-pane";
import { agentRowId } from "./inbox-threads";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

/**
 * The Inbox's Agent row names its session by the title that session stamped, and not by its id
 * (dispatch://INFRA-1368). The server answers the name (`nameAgentSessions`,
 * `api/replies_agent.go`) out of the newest title the session stamped on anything it wrote
 * (`delivery.ResolveSessionTitles`), and the band draws that name as plain text
 * (`ThreadOwnerLine`, `web/src/features/inbox/AskOwnerLink.tsx`), so what a reader sees in the
 * list is the whole of what that read decided.
 *
 * The state this is read over is the one the fix is about: a session that stamped its title on
 * its reply and then wrote again carrying none, which is what `postDeliveryReply` writes in
 * production. Before the fix the resolved read took the title off the session's newest event, so
 * the later untitled write left the map with no entry for the session and `SessionTitle` fell
 * back to the whole session id - which is what the row then showed. The id below is long and
 * says what it is, so the two outcomes cannot be read for each other.
 *
 * It is a file of its own, with a seed of its own, because neither session of the shared
 * `agent-name.ts` fixture has that shape - one stamps a title on every write and one never
 * stamps any - and that fixture's `seed()` is also the recording `agent-name.stub.e2e.ts`
 * replays, whose write order its own comment holds load-bearing.
 */

/** The session the row is read over. Long and plainly an id, so the title it stamped and the
 *  fallback to its id are nothing alike on the row. */
const BAND_SESSION_ID = "e2e-agent-name-band-session-5f21c7";

/** The title the session stamps on its reply, which is the name the band must show. */
const BAND_SESSION_TITLE = "Harbour pilot";

const BAND_SESSION: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  roles: [],
  session_id: BAND_SESSION_ID,
  title: BAND_SESSION_TITLE,
};

/**
 * One Agent row: alice's one-off message to the session, the session's reply to that delivery
 * with its title stamped on it, and then a second write from the same session carrying no title
 * at all - the write whose own event used to decide the name. The session then leaves the live
 * listener's list, so nothing live can name it and the page has only the resolved read to go on.
 *
 * Answers the message id, which is what the row is addressed by.
 */
async function seedBandRow(): Promise<string> {
  await setLiveSessions([BAND_SESSION]);
  const message = await createAgentMessage(
    BAND_SESSION_ID,
    { body: "How far is the harbour rollout?", delivery: "aside" },
    { login: "alice" }
  );
  await replyToMessageDelivery(
    message.id,
    { attempt: 1, body: "Two berths left to cut over." },
    { id: BAND_SESSION_ID, kind: "session", origin: { session_title: BAND_SESSION_TITLE } }
  );
  // The later write that carries no title: a follow-up under the reply already there, with an
  // actor holding no origin, as `postDeliveryReply` writes one.
  await replyToMessageDelivery(
    message.id,
    { attempt: 1, body: "Both berths are in." },
    { id: BAND_SESSION_ID, kind: "session" },
    { followUp: true }
  );
  await setSessionLive(BAND_SESSION_ID, false);
  return message.id;
}

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): the seed's five listener and API writes, a sign-in and the Inbox's own first draw;
// measured at 1.9 s in an issue pod on one worker.
test("the Inbox's agents band names a session by the title it stamped, not by its id", async ({
  browser,
}) => {
  const message = await seedBandRow();
  await withAlice(browser, async (page) => {
    await page.goto("/");
    const row = inboxRow(page, agentRowId(message));
    await expect(row).toBeVisible();

    // The name first, so a row naming the session by its id reports the whole line it drew.
    await expect(row).toContainText(BAND_SESSION_TITLE);
    await expect(row).not.toContainText(BAND_SESSION_ID);
    // And the name is the owner line's whole text, not a word inside a longer one.
    await expect(row.getByText(BAND_SESSION_TITLE, { exact: true })).toBeVisible();
  });
});
