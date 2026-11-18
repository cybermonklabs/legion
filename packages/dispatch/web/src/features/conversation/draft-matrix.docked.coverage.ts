/**
 * The draft matrix's coverage list, docked composer (INFRA-1503): each composer census item that
 * `draft-matrix.docked.test.tsx` answers for, held to one of its tests or to one of the numbered
 * reasons of its header, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The host as the matrix file's titles name it. */
const HOST = "docked composer";

/** The numbered reasons of the matrix file's header. */
const REASONS = {
  1: "1. Above the phone layout no thread view covers the docked composer; on a phone a comment's Reply covers it with one, and Back uncovers it, the phone row.",
  2: "2. Elsewhere needs a held refusal; the typed version is the unmount row.",
  3: "3. The docked composer sits outside every thread card, so no Collapse thread moves it.",
  4: "4. The docked composer has no anchor, so it draws no Close (asserted absent below).",
  5: "5. The issue picker is an Agents row's (`AgentMessageComposer`), and the Conversation draws no Agents row's composer.",
  6: "6. `draft-matrix.issue-page.test.tsx` makes these cells, with the issue page drawn around the Conversation: another issue's address draws the page afresh (`IssueDetail` keyed by `route.key`), and another tab hides the Conversation's panel and keeps it drawn. The composer's `owner` and send name follow `issueKey`, which only that remount changes.",
  7: "7. The Conversation draws a loading line or an error in place of the list while its reads are out or failed (`loading`, `failed`), which unmounts the docked composer as leaving the issue does, and the composer drawn when they land again is the one the issue-left row draws on return.",
  8: "8. On a phone a comment link opens the comment's thread view as it lands (`ConversationTab`'s focus effect), which covers this composer as the phone row's Reply does, and a decision landing on the comment closes the view as Back does (`phone.hide`). A message's Reply moves this composer at every width, as the rows above do.",
  9: "9. Widening the page past the phone layout closes the thread view as Back does, unless a send from it is out, which keeps it open until Back (`usePhoneThread`'s `outlived`); this composer stays mounted at the issue throughout. The Jump to latest pill, which waits under a thread view for Back, and the read mark's ref on the newest card (`register`), which moves to the view's copy of it, move no composer. The app shell's forms are the sign-in file's.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | address prop | mutationKey={sendKey}",
    { row: `${HOST} · Close, then Discard draft? · cannot make: the composer draws no Close` },
    { reason: REASONS[3] },
    { reason: REASONS[4] },
    { reason: REASONS[5] },
    { reason: REASONS[6] }
  ),
  ...answers(
    'ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | address prop | owner={{ issueKey, kind: "issue" }}',
    { reason: REASONS[6] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | address prop | replyTo={replyTo}",
    cells(`${HOST} · Reply on another message, then back`),
    cells(`${HOST} · host moves it over a refusal (elsewhere), then back`, ["refused", "stopped"]),
    {
      row: `${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused`,
    },
    cells(`${HOST} · Cancel reply`),
    cells(`${HOST} · Escape over a reply`),
    cells(`${HOST} · Escape over a comment, then Discard`),
    { reason: REASONS[2] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | conditional | <div hidden={phoneThread !== undefined}>",
    cells(`${HOST} · phone · a comment's Reply, then Back`),
    { reason: REASONS[1] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | early return | if (loading) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | early return | if (failed) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | form | <div hidden={phoneThread !== undefined}>",
    { reason: REASONS[8] }
  ),
  ...answers("ConversationTab | - | form | (phone.id === undefined) ? …", { reason: REASONS[9] }),
  ...answers("ConversationTab | - | form | if (phone.id === id)", { reason: REASONS[8] }),
  ...answers("ConversationTab | - | form | if (isPhoneViewport)", { reason: REASONS[8] }),
  ...answers(
    "ConversationTab | - | form | (visible && phoneThread === undefined && !follow.atTop && shown.length > 0) ? …",
    { reason: REASONS[9] }
  ),
  ...answers(
    "ConversationTab | - | form | (item.id === newestId && phoneThread === undefined) ? …",
    { reason: REASONS[9] }
  ),
  ...answers("usePhoneThread | - | form | if (isPhoneViewport || outlived) return …", {
    reason: REASONS[9],
  }),
  ...answers(
    "usePhoneThread | - | form | (isPhoneViewport || outlived || cardReplySending || replySending) ? …",
    { reason: REASONS[9] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | lifetime | <MentionComposer mutationKey={sendKey}>",
    cells(`${HOST} · the page reloaded, then back`)
  ),
];
