// The per-issue Legion facts' e2e fixture (dispatch://INFRA-2915), written straight into
// Postgres beside `legion-seed.ts`: a board of 24 cards in one project, where every use a held
// slot is put to and every reason a card waits is reached by some card, and eleven of the
// board's twelve columns hold a card: Waiting on children (`awaiting_children`) holds none, since
// no card is in that phase. ACME's board (`seedLegionFixture`) draws five cards, which
// states each rule at most once; a row that reads the facts or the waiting rules over a whole
// board needs one that holds every state at once.
//
// It writes no `issue_facts` row. The facts consumer writes those, and a fixture that wrote them
// would hide the consumer never running. An issue gets its row once it has a role event or a
// handoff and a pass of the consumer has read it, so a row that reads an issue's Legion work
// writes a role event on it (`writeRoleEvent`) and waits for its row (`waitForIssueFacts`).
//
// The states themselves are the server's rules, `legion.SlotUse` and `legion.CardWaiting` in
// `packages/envoy/internal/dispatch/legion/board_rules.go`. Each card below carries the state
// those rules answer for it, and the fixture returns the keys grouped by state, so a row asserts
// that the board names WIDGETS-4 held for its children rather than that some card somewhere is.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { setTimeout as sleep } from "node:timers/promises";

import { SEED_ACTOR, settledCounters, writeEventsForFacts } from "./legion-seed";
import { sql } from "./psql";

/** The project this fixture's board belongs to. It is not `legion-seed.ts`'s ACME, so a row may
 *  seed both boards and read each by its own project. */
export const LEGION_FACTS_PROJECT = "WIDGETS";

/** How long a wait for the facts consumer's row is given. The consumer is woken by every
 *  committed event (the `dispatch_events` notice the events trigger sends), starts a pass at most
 *  `legionfacts.PassGap` (10 s) after the one before it, and holds a stored row to within
 *  `legionfacts.Freshness` (60 s) of the event that moved it
 *  (`packages/envoy/internal/dispatch/legionfacts/consumer.go`). So 75 s is that minute, one more
 *  pass gap, and a few seconds for the pass itself. */
const issueFactsWait = 75_000;

/** What the slot an issue holds of its own is used for (`legion.SlotUse`). */
type SlotUse = "working" | "between_turns" | "held_children" | "held_person" | "held_idle";

/** Why a card waits (`legion.CardWaiting`). */
type WaitingReason = "ready" | "slot" | "blocked" | "person";

/** One card of the fixture's board: the issue Dispatch holds, the fields the daemon reported of
 *  it, and the state the two rules answer for it.
 *
 *  `working` and `idle` are the roles whose claim the last read found in each state, which is
 *  what makes a card's slot `working` or `between_turns`. `parent` is the issue's parent in
 *  Dispatch, which is how the tree walk reaches a root whose slot is held for its children. */
interface Card {
  readonly number: number;
  readonly title: string;
  readonly phase: string;
  readonly slot?: number;
  readonly parent?: number;
  readonly working?: readonly string[];
  readonly idle?: readonly string[];
  readonly waitingOn?: { readonly kind: string; readonly answered: boolean };
  readonly slotUse?: SlotUse;
  readonly waiting?: WaitingReason;
}

/** The board, card by card. The first seven are the five uses of a held slot: a worker in a turn
 *  (1 and 22), a slot held between turns (2, and 21 at the design gate), a slot held for a
 *  person (3), a slot held for children that work (4, whose child 5 implements) and a slot held
 *  for children that do not (6, whose child 7 is held). The four reasons a card waits follow:
 *  ready (8, 9 and 23, in the daemon's line with nothing holding them), waiting for a slot to
 *  come back (10 and 24, in the line and already in a phase of their own), blocked (11, which a
 *  blocker link holds) and waiting on a person (3 and 12). The rest fill the columns the board
 *  draws, so no column of `legion.PhaseColumns` is empty but Waiting on children, which no card's
 *  phase reaches: Retro holds 16, and Merging holds the `merging` card 17 and the
 *  `awaiting_merge` cards 6 and 18. */
const CARDS: readonly Card[] = [
  {
    number: 1,
    title: "Implement the widget loader",
    phase: "implementing",
    slot: 0,
    working: ["implementer"],
    slotUse: "working",
  },
  {
    number: 2,
    title: "Test the widget loader",
    phase: "testing",
    slot: 1,
    idle: ["tester"],
    slotUse: "between_turns",
  },
  {
    number: 3,
    title: "Ask about the gadget schema",
    phase: "awaiting_person",
    slot: 2,
    waitingOn: { kind: "ask", answered: false },
    slotUse: "held_person",
    waiting: "person",
  },
  {
    number: 4,
    title: "Accept the gadget rewrite",
    phase: "accepting",
    slot: 3,
    slotUse: "held_children",
  },
  { number: 5, title: "Rewrite the gadget store", phase: "implementing", parent: 4 },
  {
    number: 6,
    title: "Merge the sprocket metrics",
    phase: "awaiting_merge",
    slot: 4,
    slotUse: "held_idle",
  },
  { number: 7, title: "Hold the sprocket docs", phase: "held", parent: 6 },
  { number: 8, title: "Queue the cog report", phase: "admitted", waiting: "ready" },
  { number: 9, title: "Queue the cog export", phase: "admitted", waiting: "ready" },
  {
    number: 10,
    title: "Resume the cog importer",
    phase: "implementing",
    working: [],
    waiting: "slot",
  },
  { number: 11, title: "Blocked on the widget loader", phase: "admitted", waiting: "blocked" },
  {
    number: 12,
    title: "Ask about the cog naming",
    phase: "awaiting_person",
    waitingOn: { kind: "gate", answered: false },
    waiting: "person",
  },
  { number: 13, title: "Plan the sprocket intake", phase: "planning" },
  { number: 14, title: "Review the sprocket intake", phase: "reviewing" },
  { number: 15, title: "Accept the widget docs", phase: "accepting" },
  { number: 16, title: "Write the widget retro", phase: "retro" },
  { number: 17, title: "Merge the widget docs", phase: "merging" },
  { number: 18, title: "Wait for the gadget merge", phase: "awaiting_merge" },
  { number: 19, title: "Test the gadget store", phase: "testing" },
  { number: 20, title: "Hold the gadget rollout", phase: "held" },
  {
    number: 21,
    title: "Design the cog pipeline",
    phase: "admitted",
    slot: 5,
    slotUse: "between_turns",
  },
  {
    number: 22,
    title: "Review the cog pipeline",
    phase: "reviewing",
    slot: 6,
    working: ["reviewer"],
    slotUse: "working",
  },
  { number: 23, title: "Queue the sprocket audit", phase: "admitted", waiting: "ready" },
  { number: 24, title: "Resume the sprocket planner", phase: "planning", waiting: "slot" },
];

/** The daemon's waiting line, in the daemon's own order, which is neither Dispatch's key order
 *  nor its priority order. The two issues already in a phase of their own (10 and 24) keep their
 *  phase's column and carry their place in the line on the card, which is what makes them wait
 *  for a slot rather than stand in the queue. */
const WAITING = [9, 8, 23, 10, 24];

/** The issue a blocker link holds, and what holds it. */
const BLOCKED = { issue: 11, blockedBy: [1] };

/** The admission block one read of the daemon would have recorded: the cap, every issue holding
 *  a slot, the waiting line, the live tree roots and the blocked issue. */
function admission(key: (number: number) => string): string {
  return JSON.stringify({
    cap: 8,
    active: CARDS.filter((card) => card.slot !== undefined).map((card) => key(card.number)),
    waiting: WAITING.map(key),
    trees: CARDS.filter((card) => card.parent === undefined && card.slot !== undefined).map(
      (card) => key(card.number)
    ),
    blocked: [{ issue: key(BLOCKED.issue), blockedBy: BLOCKED.blockedBy.map(key) }],
  });
}

/** What this fixture wrote: the project, every card's key in the order the cards are declared,
 *  and the keys grouped by the state each one is in, so a row asserts against names and not
 *  against counts alone. A state's list holds every card the rules answer it for. */
export interface LegionFactsFixture {
  readonly project: string;
  readonly keys: readonly string[];
  readonly slotUse: Readonly<Record<SlotUse, readonly string[]>>;
  readonly waiting: Readonly<Record<WaitingReason, readonly string[]>>;
}

/** Seeds the board above into this run's database, with the project's daemon named at
 *  `daemonUrl`, and answers what it wrote.
 *
 *  Everything is one `sql` call: the project and the people row the issues are created by, the
 *  issues themselves, the `legion_settings` row that names the daemon, one good read, and every
 *  observation that read found. It writes no event, so each issue's `last_seq` stays 0, and the
 *  project's `next_number` is 25, after its last card. */
export async function seedLegionFactsFixture(daemonUrl: string): Promise<LegionFactsFixture> {
  const key = (number: number) => `${LEGION_FACTS_PROJECT}-${number}`;
  const observations: string[] = [];
  const observe = (issue: string, field: string, value: string, ago: string) => {
    observations.push(
      `('${LEGION_FACTS_PROJECT}', '${issue}', '${field}', '${value.replaceAll("'", "''")}'::jsonb, now() - interval '${ago}')`
    );
  };
  observe("", "admission", admission(key), "3 hours");
  observe("", "settings", JSON.stringify({ admission_cap: 8, review_round_cap: 3 }), "3 hours");
  for (const card of CARDS) {
    const issue = key(card.number);
    observe(issue, "phase", JSON.stringify(card.phase), `${120 - card.number} minutes`);
    if (card.slot !== undefined) {
      observe(issue, "slot", String(card.slot), "4 hours");
    }
    for (const role of card.working ?? []) {
      observe(issue, `claim.${role}`, '"working"', "30 minutes");
    }
    for (const role of card.idle ?? []) {
      observe(issue, `claim.${role}`, '"idle"', "30 minutes");
    }
    if (card.waitingOn !== undefined) {
      const since = "2026-11-09T08:00:00Z";
      const answeredAt = card.waitingOn.answered ? '"2026-11-09T09:00:00Z"' : "null";
      observe(
        issue,
        "waiting_on",
        `{"kind":"${card.waitingOn.kind}","since":"${since}","answeredAt":${answeredAt}}`,
        "2 hours"
      );
    }
  }

  await sql(
    `INSERT INTO projects (key, name, default_assignee) VALUES ('${LEGION_FACTS_PROJECT}', 'Widgets', 'alice') ON CONFLICT DO NOTHING`,
    `INSERT INTO people (email) VALUES ('alice') ON CONFLICT DO NOTHING`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, parent_key, created_by, rank, created_at) VALUES ${CARDS.map(
      (card) =>
        `('${key(card.number)}', '${LEGION_FACTS_PROJECT}', ${card.number}, '${card.title.replaceAll("'", "''")}', 'in_progress', ${card.number % 4}, ${
          card.parent === undefined ? "null" : `'${key(card.parent)}'`
        }, ${SEED_ACTOR}, '${String(card.number).padStart(4, "0")}', now() - interval '${card.number + 10} hours')`
    ).join(", ")}`,
    `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
       VALUES ('${LEGION_FACTS_PROJECT}', '${daemonUrl}', ${SEED_ACTOR}, now())`,
    `INSERT INTO legion_reads (project, read_at, ok, error, bytes)
       VALUES ('${LEGION_FACTS_PROJECT}', now() - interval '20 seconds', true, null, 8192)`,
    `INSERT INTO legion_observations (project, issue_key, field, value, observed_at) VALUES ${observations.join(", ")}`,
    ...settledCounters(LEGION_FACTS_PROJECT)
  );

  const grouped = <State extends string>(
    states: readonly State[],
    of: (card: Card) => State | undefined
  ) => {
    const groups: Record<string, readonly string[]> = {};
    for (const state of states) {
      groups[state] = CARDS.filter((card) => of(card) === state).map((card) => key(card.number));
    }
    return groups as Record<State, readonly string[]>;
  };
  return {
    project: LEGION_FACTS_PROJECT,
    keys: CARDS.map((card) => key(card.number)),
    slotUse: grouped(
      ["working", "between_turns", "held_children", "held_person", "held_idle"],
      (card) => card.slotUse
    ),
    waiting: grouped(["ready", "slot", "blocked", "person"], (card) => card.waiting),
  };
}

/** The statement that writes one event of a Legion role working `issue`, `ago` before now (a
 *  Postgres interval): a comment by a session titled `Legion <role> · <issue>`, which makes the
 *  issue carry Legion phase work and is where its work time starts (`legion.RoleSessionWorks`).
 *  The facts consumer then writes the issue's row (`waitForIssueFacts`).
 *
 *  The event takes the issue's next seq, as an event the broker writes does: one statement moves
 *  `last_seq` on by one and writes the event under it, so a later write through the API appends
 *  the event after this one. The session is one of its own per role and issue, since a session's
 *  newest title is the one the consumer reads (`session_titles`). A row that writes several hands
 *  them to one `writeEventsForFacts`, so the consumer reads them however late in the row they
 *  come. */
export function roleEventStatement(issue: string, role: string, ago: string): string {
  const actor = `{"kind":"session","id":"s-${role}-${issue}","origin":{"session_title":"Legion ${role} · ${issue}"}}`;
  return `WITH next AS (UPDATE issues SET last_seq = last_seq + 1 WHERE key = '${issue}' RETURNING last_seq)
     INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at)
       SELECT '${issue}', last_seq, 'comment.created', '${actor}'::jsonb, '{}'::jsonb, false,
         now() - interval '${ago}'
       FROM next`;
}

/** Writes one role event (`roleEventStatement`) through `writeEventsForFacts`, so the consumer
 *  reads it however late in the row it comes. */
export async function writeRoleEvent(issue: string, role: string, ago: string): Promise<void> {
  await writeEventsForFacts(roleEventStatement(issue, role, ago));
}

/** Waits until the facts consumer has written the `issue_facts` row of every one of `issues`, for
 *  up to `issueFactsWait` (the consumer's pace), and fails naming the issues still without one.
 *  Each issue needs a role event or a handoff first (`writeRoleEvent`). */
export async function waitForIssueFacts(issues: readonly string[]): Promise<void> {
  const wanted = issues.map((issue) => `'${issue}'`).join(", ");
  const deadline = Date.now() + issueFactsWait;
  for (;;) {
    const written = new Set(
      (await sql(`SELECT issue_key FROM issue_facts WHERE issue_key IN (${wanted})`))
        .split("\n")
        .filter((line) => line !== "")
    );
    const missing = issues.filter((issue) => !written.has(issue));
    if (missing.length === 0) return;
    if (Date.now() > deadline) {
      throw new Error(
        `the facts consumer wrote no issue_facts row of ${missing.join(", ")} within ${issueFactsWait / 1000} s`
      );
    }
    // A poll's pause between two reads of `issue_facts`, bounded by the deadline above: a row
    // lands when a consumer pass ends, and a read each quarter second finds it soon after.
    await sleep(250);
  }
}
