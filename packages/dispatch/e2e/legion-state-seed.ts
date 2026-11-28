// One issue's Legion state, written into Postgres as Dispatch's sampler writes a read of the
// issue's project's daemon (`packages/envoy/internal/dispatch/legion/sampler.go`): the project's
// daemon setting, the fields of the issue and the project's admission block that moved, the read
// itself, and the notification an open page's live view of the issue moves on. No daemon runs in
// the suite, so this is the one way a row gives an issue a Legion state.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { sql } from "./psql";
import { sqlLiteral } from "./seed";

/** The daemon's state address: a port nothing listens on, so the sampler's own reads, one a
 *  minute, fail at once and write no observation. */
const NOTHING_ANSWERS = "http://127.0.0.1:9";

/** How far ahead of the database's clock a seeded read is dated. The sampler records each of its
 *  own failed reads at its real time, once a minute, so a read dated this far ahead stays the
 *  project's newest read for as long as a row runs, and the block draws the read the row seeded. */
const READ_AHEAD = "interval '1 hour'";

/** The admission block of the issue's project, in the daemon's own names
 *  (`legion.Admission`, `packages/envoy/internal/dispatch/legion/state.go`). */
export interface LegionAdmissionSeed {
  /** The waiting line in its order: an issue's place is its index plus one. */
  readonly waiting?: readonly string[];
  /** The issues each blocker link holds, with the issues it waits on. */
  readonly blocked?: readonly { readonly issue: string; readonly blockedBy: readonly string[] }[];
}

/** One issue's Legion state, as the daemon reported it. */
export interface LegionStateSeed {
  /** Whether the issue's project names a daemon. False writes no setting, so the issue's read
   *  answers `configured` false whatever else this holds; the project must name none yet. True
   *  where left out. */
  readonly configured?: boolean;
  /** The issue's phase, or null for an issue the daemon does not record. */
  readonly phase: string | null;
  /** The admission slot the issue holds, or null (left out) for none. */
  readonly slot?: number | null;
  /** What the issue waits on from a person, the daemon's object as it stores it, in its own
   *  names (`kind`, `ask`, `artifactId`, `gateVersion`, `since`, `answeredAt`, `answeredBy`), or
   *  null (left out) for none. A member is written as given, markup and negative numbers
   *  included. */
  readonly waitingOn?: Readonly<Record<string, unknown>> | null;
  /** Each role with a claim on the issue, and its claim's state. */
  readonly claims?: Readonly<Record<string, string>>;
  /** The project's admission block, or null (left out) for none. */
  readonly admission?: LegionAdmissionSeed | null;
  /** How the read went: a good read (left out), or a failed one with the daemon's error. */
  readonly read?: { readonly ok: true } | { readonly ok: false; readonly error: string };
}

/**
 * Writes `key`'s Legion state as one read of its project's daemon that `legion.RecordRead`
 * records (`packages/envoy/internal/dispatch/legion/store.go`):
 *
 * - the project's daemon setting in `legion_settings`, named with an address nothing answers,
 *   where `configured` is not false and the project names none yet;
 * - one `legion_observations` row for each field that moved against `legion_current`, which the
 *   trigger `legion_observations_current` keeps: the issue's `phase`, `slot`, `waiting_on` and
 *   `claim.<role>`, each the JSON the sampler stores (`legion.State.Observations`), and the
 *   project's `admission` under the empty issue key; a `null` row for each field whose newest row
 *   holds a value and that this state does not carry, a claim gone among them
 *   (`recordObservations`). A phase stored over another is one Dispatch saw start, so a row seeds
 *   the phase before first where its block reads "since" rather than "since before";
 * - a `legion_reads` row that says how the read went, in the same statement, so the two commit
 *   together as `RecordRead`'s transaction makes them, dated `READ_AHEAD` on;
 * - and `select pg_notify('dispatch_legion_state', '<PROJECT>')` (`legion.NotifyRead`), which
 *   wakes every open live view of the project's issues.
 */
export async function seedLegionState(key: string, legion: LegionStateSeed): Promise<void> {
  const project = key.slice(0, key.indexOf("-"));
  if (legion.configured !== false) {
    await sql(
      `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
         VALUES (${sqlLiteral(project)}, ${sqlLiteral(NOTHING_ANSWERS)}, '{"kind":"user","id":"alice"}', now())
         ON CONFLICT (project) DO NOTHING`
    );
  }
  const issueFields: (readonly [string, unknown])[] = [
    ["phase", legion.phase],
    ["slot", legion.slot ?? null],
    ["waiting_on", legion.waitingOn ?? null],
    ...Object.entries(legion.claims ?? {}).map(
      ([role, state]) => [`claim.${role}`, state] as const
    ),
  ];
  const found = [
    ...issueFields.map(([field, value]) => [key, field, value] as const),
    ["", "admission", legion.admission ?? null] as const,
  ]
    .map(
      ([issue, field, value]) =>
        `(${sqlLiteral(issue)}, ${sqlLiteral(field)}, ${sqlLiteral(JSON.stringify(value))}::jsonb)`
    )
    .join(", ");
  const read = legion.read ?? { ok: true };
  const projectLiteral = sqlLiteral(project);
  await sql(
    `WITH found (issue_key, field, value) AS (VALUES ${found}),
     newest AS (
       SELECT issue_key, field, value FROM legion_current
        WHERE project = ${projectLiteral}
          AND (issue_key = ${sqlLiteral(key)} OR (issue_key = '' AND field = 'admission'))
     ), written AS (
       INSERT INTO legion_observations (project, issue_key, field, value, observed_at)
       SELECT ${projectLiteral}, coalesce(found.issue_key, newest.issue_key),
              coalesce(found.field, newest.field), coalesce(found.value, 'null'::jsonb), now()
         FROM found
         FULL JOIN newest ON newest.issue_key = found.issue_key AND newest.field = found.field
        WHERE coalesce(newest.value, 'null'::jsonb) IS DISTINCT FROM coalesce(found.value, 'null'::jsonb)
       RETURNING 1
     )
     INSERT INTO legion_reads (project, read_at, ok, error, bytes)
     VALUES (${projectLiteral}, now() + ${READ_AHEAD}, ${read.ok}, ${
       read.ok ? "NULL" : sqlLiteral(read.error)
     }, ${Buffer.byteLength(JSON.stringify(legion))})`,
    `SELECT pg_notify('dispatch_legion_state', ${projectLiteral})`
  );
}
