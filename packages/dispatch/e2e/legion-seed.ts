// The Legion fixtures of the browser suite, written straight into Postgres. Two surfaces read
// them: the Settings page's Legion daemon field, where a project's daemon is named, listed and
// removed (`legion_settings`), and the project's Issues tab, which reads the daemon's board
// (`GET /api/v1/projects/{key}/legion/board` and the `legion` live view) for a project whose
// setting names a daemon. The board reads only what the sampler recorded of that daemon: its
// reads (`legion_reads`) and the fields each read found (`legion_observations`, whose trigger
// keeps `legion_current`, the newest row of each field, which the board's cards are made from:
// migration 20261117022936). So these are the rows a minute of reading would have left, without
// a daemon to read.
//
// A sampler pass that runs while a row runs changes no observation. `startFakeLegionDaemon`
// answers 404 to every route, its state route included, so every pass records a failed read,
// which the board's header reads and which sends each open board a new frame. The sampler reads
// at its start and each minute after (`legion.SampleInterval`).
//
// Nothing here writes a per-issue facts row (`issue_facts`). The facts consumer writes those
// from an issue's role events and handoffs, and `legion-facts-seed.ts` holds the helpers that
// write a role event and wait for the consumer's row.
//
// Every issue a seed here writes carries the `last_seq` of its newest event, and its project the
// `next_number` after its highest issue (`settledCounters`). A later write through the API then
// appends the next event and creates the next issue, where a counter left at its default makes
// the write reuse a seeded seq or key, which Postgres refuses.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { sql } from "./psql";

/** The actor the Legion seeds and the Issues tab's rows write their issues and settings by SQL
 *  as: alice, as the SQL text of its JSON. */
export const SEED_ACTOR = `'{"kind":"user","id":"alice"}'`;

/** The project the Settings section's daemon field names a daemon for, and whose board the
 *  fixture below seeds. */
export const LEGION_PROJECT = "ACME";

/** The project and its person, which the Settings page's `Legion project` picker lists and the
 *  daemon routes write under. No daemon is named: the spec about naming one starts from here. */
export async function seedLegionProject(): Promise<void> {
  await sql(
    `INSERT INTO projects (key, name, default_assignee) VALUES ('${LEGION_PROJECT}', 'Acme', 'alice') ON CONFLICT DO NOTHING`,
    `INSERT INTO people (email) VALUES ('alice') ON CONFLICT DO NOTHING`
  );
}

/** `seedLegionProject` with the project's daemon already named at `stateUrl`, so the Settings
 *  list shows it and its Remove has a row to take away. It writes ACME's `legion_settings` row,
 *  as `seedLegionFixture` does, so a row calls one of the two and never both. */
export async function seedLegionDaemon(stateUrl: string): Promise<void> {
  await seedLegionProject();
  await sql(
    `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
       VALUES ('${LEGION_PROJECT}', '${stateUrl}', ${SEED_ACTOR}, now())`
  );
}

/** The statements that leave `project`'s counters where a later API write expects them: each of
 *  its issues at the `last_seq` of its newest event, which is the seq the issue's next event
 *  takes one past (the event broker's `update issues set last_seq = last_seq + 1`), and the
 *  project at the `next_number` after its highest issue, which the next issue it creates takes
 *  (issue_create.go). A seed runs them once its issues and events are written. */
export function settledCounters(project: string): string[] {
  return [
    `UPDATE issues SET last_seq = coalesce((SELECT max(seq) FROM events WHERE events.issue_key = issues.key), 0)
       WHERE project_key = '${project}'`,
    `UPDATE projects SET next_number = greatest(next_number,
         coalesce((SELECT max(number) FROM issues WHERE issues.project_key = '${project}'), 0) + 1)
       WHERE key = '${project}'`,
  ];
}

/** The facts consumer's lease (`legionfacts.Lease`), as `duty.Exclusive` takes it for each pass:
 *  a two-key advisory lock, the loops' namespace (`leaseNamespace`, "LOOP") and the hashtext of
 *  the lease's name (`packages/envoy/internal/dispatch/duty/exclusive.go`). */
const FACTS_LEASE = `${0x4c4f4f50}, hashtext('legion-facts')`;

/** Writes `statements`, which add events to the log, so that the facts consumer reads every
 *  event of this row, these among them: in one transaction under the consumer's lease, with its
 *  events cursor put back at the log's start.
 *
 *  The cursor is the reason. The consumer reads the event log by a global id cursor up to the
 *  safe horizon (`readEventPage` in `legionfacts/feeds.go`), and every committed event wakes it,
 *  a direct insert too (the `events_notify` trigger, migration 0107). But the reset before each
 *  row (`e2e/seed.ts`) empties the events and restarts their ids at 1 under that cursor, and the
 *  consumer keeps no check of its own against a reset. The event relay has one
 *  (`eventLogWasReset` in `api/event_stream_relay.go`), which forgets the server's horizon, but
 *  it runs only once an event wakes the relay. Until then a pass that finds no cursor starts the
 *  feed at the horizon the server holds (`readFeedHeads`), the old log's, and a pass that read the
 *  cursor before the reset writes that position back after it. Either leaves the cursor above
 *  every id the new log hands out, so no event of the row is read until the ids climb past it. A
 *  project's first whole read still sees the events written before it, and nothing sees the ones
 *  after: the first role event of a row got its facts, and a later one never did. At 0 the next
 *  pass reads every event of the row from the start of the new log: each names an issue the
 *  pass computes again, and the pass writes no row whose values stand (`upsert` in
 *  `legionfacts/store.go`).
 *
 *  The lease keeps a pass out of the transaction: a pass that read the cursor before it would
 *  write its old position back when it ended. The lock waits for a running pass to end, and a
 *  pass that tries for the lease meanwhile tries again a pass gap later. */
export async function writeEventsForFacts(...statements: string[]): Promise<void> {
  await sql(
    "BEGIN",
    `SELECT pg_advisory_xact_lock(${FACTS_LEASE})`,
    ...statements,
    `INSERT INTO issue_facts_cursor (name, position) VALUES ('events', 0)
       ON CONFLICT (name) DO UPDATE SET position = 0`,
    "COMMIT"
  );
}

/** One observation row: the field of one issue (or of the project, under the empty key) as a read
 *  found it, and how long ago that read was. */
export interface Observation {
  readonly issue: string;
  readonly field: string;
  readonly value: string;
  /** A Postgres interval: how long before now the value was first seen. */
  readonly ago: string;
}

/** The daemon's admission block: a cap of four with two slots held, two roots waiting in the
 *  daemon's own order, which is neither Dispatch's key order nor its priority order, and one
 *  root a blocker link holds. */
const ADMISSION = {
  cap: 4,
  active: ["ACME-1", "ACME-2"],
  waiting: ["ACME-4", "ACME-3"],
  trees: ["ACME-1", "ACME-2"],
  blocked: [{ issue: "ACME-5", blockedBy: ["ACME-1"] }],
};

/** The settings the daemon reports, which the board answers as they stand. */
const SETTINGS = { admission_cap: 4, review_round_cap: 3 };

const OBSERVATIONS: readonly Observation[] = [
  { issue: "", field: "admission", value: JSON.stringify(ADMISSION), ago: "3 hours" },
  { issue: "", field: "settings", value: JSON.stringify(SETTINGS), ago: "3 hours" },
  // ACME-1 holds slot 0 and is being implemented, with its implementer in a turn.
  { issue: "ACME-1", field: "phase", value: '"implementing"', ago: "2 hours" },
  { issue: "ACME-1", field: "slot", value: "0", ago: "3 hours" },
  { issue: "ACME-1", field: "claim.implementer", value: '"working"', ago: "20 minutes" },
  // ACME-2 holds slot 1 and waits on a person.
  { issue: "ACME-2", field: "phase", value: '"awaiting_person"', ago: "45 minutes" },
  { issue: "ACME-2", field: "slot", value: "1", ago: "3 hours" },
  {
    issue: "ACME-2",
    field: "waiting_on",
    value: JSON.stringify({ kind: "ask", since: "2026-11-03T04:19:00Z", answeredAt: null }),
    ago: "45 minutes",
  },
  // The two roots in the waiting line, and the one a blocker holds: each is in the queue, so
  // none of them has a phase of its own yet.
  { issue: "ACME-3", field: "phase", value: '"admitted"', ago: "90 minutes" },
  { issue: "ACME-4", field: "phase", value: '"admitted"', ago: "80 minutes" },
  { issue: "ACME-5", field: "phase", value: '"admitted"', ago: "70 minutes" },
];

/** The project, its five issues, one good read and every observation that read found: everything
 *  the board draws except the `legion_settings` row that names the daemon. The issues are
 *  Dispatch's own, which is where each card's title and priority come from. It writes no event,
 *  so each issue's `last_seq` stays 0, which is the seq its next event takes one past.
 *
 *  The board it makes: ACME-4, ACME-3 and ACME-5 in the Queue, in that order, the first two at
 *  places 1 and 2 of the daemon's line and ready, ACME-5 blocked by ACME-1; ACME-1 in
 *  Implementing, its slot working; ACME-2 in Waiting on a person, its slot held for the person
 *  since the ask began. The header counts 2 of 4 slots in use, 2 trees and 3 in the queue. */
async function seedLegionBoardProject(): Promise<void> {
  await seedLegionProject();
  await sql(
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank) VALUES
       ('ACME-1', '${LEGION_PROJECT}', 1, 'Ship the widgets', 'in_progress', 0, ${SEED_ACTOR}, 'U'),
       ('ACME-2', '${LEGION_PROJECT}', 2, 'Ask about the gadgets', 'in_progress', 1, ${SEED_ACTOR}, 'V'),
       ('ACME-3', '${LEGION_PROJECT}', 3, 'Queued behind the widgets', 'todo', 2, ${SEED_ACTOR}, 'W'),
       ('ACME-4', '${LEGION_PROJECT}', 4, 'Queued first by the daemon', 'todo', 3, ${SEED_ACTOR}, 'X'),
       ('ACME-5', '${LEGION_PROJECT}', 5, 'Held by a blocker', 'todo', 3, ${SEED_ACTOR}, 'Y')`,
    `INSERT INTO legion_reads (project, read_at, ok, error, bytes)
       VALUES ('${LEGION_PROJECT}', now() - interval '20 seconds', true, null, 4096)`,
    `INSERT INTO legion_observations (project, issue_key, field, value, observed_at) VALUES ${OBSERVATIONS.map(
      (observation) =>
        `('${LEGION_PROJECT}', '${observation.issue}', '${observation.field}', '${observation.value.replaceAll("'", "''")}'::jsonb, now() - interval '${observation.ago}')`
    ).join(", ")}`,
    ...settledCounters(LEGION_PROJECT)
  );
}

/** `seedLegionBoardProject` with the project's daemon named at `stateUrl`, which is what every row
 *  that reads ACME's board seeds. It writes ACME's `legion_settings` row, as `seedLegionDaemon`
 *  does, so a row calls one of the two and never both. */
export async function seedLegionFixture(stateUrl: string): Promise<void> {
  await seedLegionBoardProject();
  await sql(
    `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
       VALUES ('${LEGION_PROJECT}', '${stateUrl}', ${SEED_ACTOR}, now())`
  );
}

/** ACME's closed issues with Legion work: one Legion tree that finished, written as two and a
 *  half days of reading and of Dispatch's own event log would have left it
 *  (`packages/envoy/internal/dispatch/legion/metrics`). It writes into ACME, so a row seeds
 *  `seedLegionFixture` first.
 *
 *  The root ACME-6 was designed by an architect and closed; its child ACME-7, which that
 *  architect filed while designing it, was implemented, reviewed, sent back, implemented again
 *  and closed by the merger. So the facts consumer gives each a row: a work time, hours on the
 *  architect, implementer, reviewer and merger bars, one review round and a send-back for the
 *  child, and a filing in the tree at design. The two are on no card of the board, since their
 *  phase is `done`, and in no tree a card stands in, so the board's `issues` map carries neither
 *  and the items route of `legion-work-time` serves both, by the day each closed.
 *
 *  ACME-6 ends at `last_seq` 2 and ACME-7 at 5, the seqs of their newest events. The rows go in
 *  through `writeEventsForFacts`, so the facts consumer reads these events whether or not it has
 *  already read ACME whole. */
export async function seedLegionMetricsCorpus(): Promise<void> {
  const session = (role: string, issue: string) =>
    `'{"kind":"session","id":"s-${role}","origin":{"session_title":"Legion ${role} · ${issue}"}}'`;
  const root = "ACME-6";
  const child = "ACME-7";
  const event = (
    issue: string,
    seq: number,
    type: string,
    actor: string,
    payload: string,
    ago: string
  ) =>
    `INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at)
       VALUES ('${issue}', ${seq}, '${type}', ${actor}::jsonb, '${payload}'::jsonb, false, now() - interval '${ago}')`;
  const observed = (issue: string, field: string, value: string, ago: string) =>
    `INSERT INTO legion_observations (project, issue_key, field, value, observed_at)
       VALUES ('${LEGION_PROJECT}', '${issue}', '${field}', '${value}'::jsonb, now() - interval '${ago}')`;
  await writeEventsForFacts(
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank, created_at, closed_at) VALUES
       ('${root}', '${LEGION_PROJECT}', 6, 'The finished root', 'done', 1, ${SEED_ACTOR}, 'Z', now() - interval '60 hours', now() - interval '20 hours'),
       ('${child}', '${LEGION_PROJECT}', 7, 'The finished child', 'done', 2, ${SEED_ACTOR}, 'Za', now() - interval '50 hours', now() - interval '24 hours')`,
    `UPDATE issues SET parent_key = '${root}' WHERE key = '${child}'`,
    // The root's design, the child it filed while designing, and the root's own close.
    event(root, 1, "comment.created", session("architect", root), "{}", "58 hours"),
    event(
      child,
      1,
      "issue.created",
      session("architect", root),
      `{"status":"triage","parent":"${root}"}`,
      "50 hours"
    ),
    event(root, 2, "issue.closed", session("architect", root), '{"status":"done"}', "20 hours"),
    // The child's own workers, and the round it was sent back in.
    event(child, 2, "comment.created", session("implementer", child), "{}", "48 hours"),
    event(
      child,
      3,
      "issue.updated",
      session("reviewer", child),
      '{"status":"needs_review"}',
      "40 hours"
    ),
    event(
      child,
      4,
      "issue.updated",
      session("implementer", child),
      '{"status":"in_progress"}',
      "36 hours"
    ),
    event(child, 5, "issue.closed", session("merger", child), '{"status":"done"}', "24 hours"),
    `INSERT INTO handoffs (issue_key, phase, version, body, author, created_at)
       VALUES ('${child}', 'implement', 1, 'done', ${session("implementer", child)}::jsonb, now() - interval '42 hours'),
              ('${child}', 'review', 1, 'accepted', ${session("reviewer", child)}::jsonb, now() - interval '30 hours')`,
    // What the sampler recorded of the two while they ran.
    observed(root, "phase", '"admitted"', "59 hours"),
    observed(root, "slot", "2", "59 hours"),
    observed(root, "phase", '"planning"', "52 hours"),
    observed(root, "phase", '"done"', "20 hours"),
    observed(child, "phase", '"implementing"', "48 hours"),
    observed(child, "claim.implementer", '"working"', "48 hours"),
    observed(child, "phase", '"reviewing"', "40 hours"),
    observed(child, "claim.implementer", '"idle"', "40 hours"),
    observed(child, "phase", '"implementing"', "36 hours"),
    observed(child, "phase", '"merging"', "30 hours"),
    observed(child, "phase", '"done"', "24 hours"),
    ...settledCounters(LEGION_PROJECT)
  );
}

/** What a read of ACME's daemon a minute later leaves behind: a good read now, the value it found
 *  for one field, and the notice on `legion.StateChannel` that wakes every open board of the
 *  project and the facts consumer (`packages/envoy/internal/dispatch/legion/store.go`). `value`
 *  is the field's JSON. */
export async function pushLegionObservation(
  issue: string,
  field: string,
  value: string
): Promise<void> {
  await sql(
    `INSERT INTO legion_reads (project, read_at, ok, error, bytes)
       VALUES ('${LEGION_PROJECT}', now(), true, null, 4096)`,
    `INSERT INTO legion_observations (project, issue_key, field, value, observed_at)
       VALUES ('${LEGION_PROJECT}', '${issue}', '${field}', '${value.replaceAll("'", "''")}'::jsonb, now())`,
    `SELECT pg_notify('dispatch_legion_state', '${LEGION_PROJECT}')`
  );
}

/** What a failed read of `project`'s daemon leaves behind, as the sampler writes one
 *  (`legion.RecordFailedRead`): a `legion_reads` row now, with `ok` false and `error` its words,
 *  and the notice on `legion.StateChannel` that sends every open board of the project a new frame.
 *  The board's header then names the error, and the Issues tab draws it in the alert under its
 *  tabs. A row that measures a position on a project with a daemon seeds one first, so the alert
 *  is there from the start rather than arriving with the sampler's own failed read of
 *  `startFakeLegionDaemon`, which lands within a minute. */
export async function seedFailedLegionRead(project: string, error: string): Promise<void> {
  await sql(
    `INSERT INTO legion_reads (project, read_at, ok, error, bytes)
       VALUES ('${project}', now(), false, '${error.replaceAll("'", "''")}', 0)`,
    `SELECT pg_notify('dispatch_legion_state', '${project}')`
  );
}

/** What reads of ACME's daemon over the past days leave behind: each of `observations`, first seen
 *  `ago` before now, then a good read now and the notice that wakes every open board of the
 *  project and the facts consumer, as `pushLegionObservation` sends for one field. A row that
 *  needs a card in a phase for a given time, or a phase history the facts count review rounds
 *  from, seeds it here. */
export async function pushLegionObservations(observations: readonly Observation[]): Promise<void> {
  await sql(
    `INSERT INTO legion_observations (project, issue_key, field, value, observed_at) VALUES ${observations
      .map(
        (observation) =>
          `('${LEGION_PROJECT}', '${observation.issue}', '${observation.field}', '${observation.value.replaceAll("'", "''")}'::jsonb, now() - interval '${observation.ago}')`
      )
      .join(", ")}`,
    `INSERT INTO legion_reads (project, read_at, ok, error, bytes)
       VALUES ('${LEGION_PROJECT}', now(), true, null, 4096)`,
    `SELECT pg_notify('dispatch_legion_state', '${LEGION_PROJECT}')`
  );
}

/** A Legion daemon as Dispatch's sampler meets it: a listener that answers 404 to every route,
 *  its state route (`GET /legion/v1/state`) included. So every pass of the sampler records a
 *  failed read of it and changes no observation, and the board draws what the seed wrote. It
 *  listens on a port the OS chooses, on the loopback the Dispatch server reaches it by. */
export interface FakeLegionDaemon {
  /** The base URL a project's `legion_settings` row names. */
  readonly url: string;
  close(): Promise<void>;
}

export async function startFakeLegionDaemon(): Promise<FakeLegionDaemon> {
  const server: Server = createServer((_incoming, response) => {
    response.writeHead(404, { "content-type": "application/json" }).end("{}");
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  const { port } = server.address() as AddressInfo;
  return {
    close: () => {
      const closed = Promise.withResolvers<void>();
      server.close((error) => (error === undefined ? closed.resolve() : closed.reject(error)));
      return closed.promise;
    },
    url: `http://127.0.0.1:${port}`,
  };
}
