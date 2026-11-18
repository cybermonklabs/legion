/**
 * The draft matrix's coverage list, Agents row (INFRA-1503): each composer census item that
 * `draft-matrix.agents-row.test.tsx` answers for, held to one of its tests or to one of the
 * numbered reasons of its header, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The host as the matrix file's titles name it. */
const HOST = "Agents row";

/** The numbered reasons of the matrix file's header. */
const REASONS = {
  1: "1. The Agents page has no thread view and no Back of its own; the row's composer stays on the page.",
  2: "2. Elsewhere needs a held refusal; the typed version is the unmount row.",
  3: "3. The row's composer has no anchor, so it draws no Close (asserted absent below).",
  4: "4. A row draws its details, and the composer in them, from the first time the reader opens it (`opened`, from `openedRows`, which only gains rows), so that conditional starts the mount and never ends one.",
  5: "5. A row the reader has opened stays mounted, hidden, when its group is folded, the filters leave it out, or the window scrolls past it (`keptAside`, the row's `hidden`), in the same parent under the same key, so nothing its composer holds moves.",
  6: "6. A session that leaves the live list takes its row with it, and a page with no session lists none (`isPending`, `isError`, an empty list); the row drawn when the session is back is the one the page-left row's return draws, at the same session's send name.",
  7: "7. The windowed list draws a group's heading for an item of kind `group` and a row for one of kind `row` (`renderItem`), and each item's kind is fixed where `items` builds it under a key of its own (`group:<name>` or the session), so no row is ever drawn as a heading in its place.",
  8: "8. The layout changes one thing in a row: from `md` its directory shows the path's last segment and below it the whole path (`mdViewport`). The row, its details and its composer are drawn the same at every width, so a phone makes the rows above as they are. The app shell's forms are the sign-in file's.",
} as const;

/** What a lifetime item answers to until the reload rows of this round are written. */
const PENDING = { reason: "pending: the reload rows of this round" };

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "AgentMessageComposer | MentionComposer | address prop | mutationKey={sendKey}",
    {
      row: `${HOST} · Close, then Discard draft? · cannot make: the row's composer draws no Close`,
    },
    { reason: REASONS[1] },
    { reason: REASONS[3] },
    { reason: REASONS[6] }
  ),
  ...answers(
    "AgentMessageComposer | MentionComposer | address prop | ownMoves={commits}",
    cells(`${HOST} · Agents pick`)
  ),
  ...answers(
    "AgentMessageComposer | MentionComposer | address prop | owner={composerOwner}",
    cells(`${HOST} · Agents pick`)
  ),
  ...answers(
    "AgentMessageComposer | MentionComposer | address prop | replyTo={replyTo?.target ?? null}",
    cells(`${HOST} · Reply on another message, then back`),
    cells(`${HOST} · host moves it over a refusal (elsewhere), then back`, ["refused", "stopped"]),
    {
      row: `${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused`,
    },
    cells(`${HOST} · Cancel reply`),
    cells(`${HOST} · Escape over a reply`),
    cells(`${HOST} · Escape over a message, then Discard`),
    { reason: REASONS[2] }
  ),
  ...answers(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the key holds the expression's own source text, whose template literal this is.
    "AgentMessageComposer | MentionComposer | address prop | seedMentions={ useDirectChannel ? undefined : [{ target: `session:${agent.session_id}`, title: agent.title || agent.session_id }] }",
    cells(`${HOST} · Agents pick`)
  ),
  ...answers(
    "AgentRowDetails | AgentMessageComposer | conditional | <div hidden={!expanded}>",
    cells(`${HOST} · row collapsed by the host, then opened`)
  ),
  ...answers("AgentRowDetails | AgentMessageComposer | address prop | agent={agent}", {
    reason: REASONS[6],
  }),
  ...answers(
    "AgentRowDetails | AgentMessageComposer | address prop | replyTo={replyTo}",
    cells(`${HOST} · Reply on another message, then back`),
    cells(`${HOST} · Cancel reply`)
  ),
  ...answers("AgentRowDetails | AgentMessageComposer | address prop | sendKey={sendKey}", {
    reason: REASONS[6],
  }),
  ...answers("AgentRow | AgentRowDetails | conditional | (opened) ? …", { reason: REASONS[4] }),
  ...answers("AgentRow | AgentRowDetails | conditional | <article hidden={hidden}>", {
    reason: REASONS[5],
  }),
  ...answers("AgentRow | AgentRowDetails | address prop | agent={agent}", { reason: REASONS[6] }),
  ...answers(
    "AgentRow | AgentRowDetails | address prop | expanded={expanded}",
    cells(`${HOST} · row collapsed by the host, then opened`)
  ),
  ...answers(
    "AgentRow | AgentRowDetails | address prop | replyTo={replyTo}",
    cells(`${HOST} · Reply on another message, then back`)
  ),
  ...answers(
    "AgentsPage | AgentRow | key | <AgentRow key={agent.session_id}>",
    { reason: REASONS[5] },
    { reason: REASONS[6] }
  ),
  ...answers("AgentsPage | AgentRow | key | <div key={agent.session_id}>", { reason: REASONS[5] }),
  ...answers(
    "AgentsPage | AgentRow | conditional | drawn by grouped[group] .filter((agent) => kept.has(agent.session_id) && !(shown[group] && matches(agent, group))) .map(…)",
    { reason: REASONS[5] },
    { reason: REASONS[6] }
  ),
  ...answers("AgentsPage | AgentRow | conditional | drawn by AGENT_GROUPS.flatMap(…)", {
    reason: REASONS[5],
  }),
  ...answers("AgentsPage | AgentRow | conditional | !(agents.length === 0) ? …", {
    reason: REASONS[6],
  }),
  ...answers("AgentsPage | AgentRow | early return | if (isPending) return …", {
    reason: REASONS[6],
  }),
  ...answers(
    "AgentsPage | AgentRow | early return | if (isError && agents.length === 0 && !refilling) return …",
    { reason: REASONS[6] }
  ),
  ...answers('AgentsPage | AgentRow | conditional | !(item.kind === "group") ? …', {
    reason: REASONS[7],
  }),
  ...answers("WindowedList | AgentRow | key | <div key={item.key}>", { reason: REASONS[7] }),
  ...answers("WindowedList | AgentRow | early return | if (item === undefined) return …", {
    reason: REASONS[5],
  }),
  ...answers("WindowedList | AgentRow | conditional | drawn by list.drawn.flatMap(…)", {
    reason: REASONS[5],
  }),
  ...answers("AgentsPage | AgentRow | address prop | agent={agent}", { reason: REASONS[6] }),
  ...answers(
    "AgentsPage | AgentRow | address prop | expanded={expandedRows.has(agent.session_id)}",
    cells(`${HOST} · row collapsed by the host, then opened`)
  ),
  ...answers("AgentsPage | AgentRow | address prop | hidden={hidden}", { reason: REASONS[5] }),
  ...answers("AgentsPage | AgentRow | address prop | opened={openedRows.has(agent.session_id)}", {
    reason: REASONS[4],
  }),
  ...answers(
    'AppShell | AgentsPage | route | <Route path="/agents">',
    cells(`${HOST} · the Agents page left, then back`)
  ),
  ...answers("AgentsPage | AgentRow | form | mdViewport={mdViewport}", { reason: REASONS[8] }),
  ...answers("AgentRow | - | form | (mdViewport) ? …", { reason: REASONS[8] }),
  ...answers(
    "AgentMessageComposer | MentionComposer | lifetime | <MentionComposer mutationKey={sendKey}>",
    PENDING
  ),
];
