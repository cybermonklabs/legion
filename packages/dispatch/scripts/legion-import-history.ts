/**
 * Import a project's Dispatch history into a local Dispatch (dispatch://INFRA-979 prototype): the
 * events, handoffs and asks a production fetch dumped, with their original times, actors and
 * session titles, so the Legion metrics compute over a local copy of the project's issues.
 *
 *   bun run packages/dispatch/scripts/legion-import-history.ts --dsn <local postgres> \
 *     --project INFRA --dump <dir or .tar.gz> [--dump <second part> ...]
 *
 * A dump holds issues/<KEY>.json files of {issue, events, handoffs, asks}, as the production
 * read routes answer them (GET /api/v1/issues/<KEY>, …/events, …/handoffs, …/asks). Issues are
 * matched by key and none is created; the keys with no local issue are reported. A matched issue
 * whose local row has no close time takes the dump's, since the metrics read closed_at. Running it
 * twice imports nothing twice: events keep their (issue, seq), handoffs and asks their ids.
 *
 * Handoff bodies are not in the list route, so an imported handoff's body is empty. Events are
 * written as already published (notify false, published_at their own time), so the local outbox
 * sends none of them on.
 */
import { mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";

import { $, SQL } from "bun";

const { values: args } = parseArgs({
  options: {
    dsn: { type: "string" },
    project: { type: "string" },
    dump: { type: "string", multiple: true },
  },
});
if (!args.dsn || !args.project || !args.dump?.length) {
  throw new Error("--dsn, --project and at least one --dump are required");
}
const project = args.project;

interface DumpEvent {
  seq: number;
  type: string;
  actor: unknown;
  payload: unknown;
  created_at: string;
}
interface DumpHandoff {
  id: string;
  phase: string;
  version: number;
  author: unknown;
  created_at: string;
}
interface DumpAsk {
  id: string;
  author: unknown;
  question: string;
  options: unknown[];
  multiple: boolean;
  urgency: string;
  state: string;
  answer: unknown;
  resolution?: unknown;
  kind: string;
  approval: unknown;
  created_at: string;
  edited_at: string | null;
}
interface DumpIssue {
  issue: { key: string; closed_at: string | null };
  events: DumpEvent[];
  handoffs: DumpHandoff[];
  asks: DumpAsk[];
}

/** Every issue record of every dump, by key; a .tar.gz is unpacked under $TMPDIR first. */
async function readDumps(paths: readonly string[]): Promise<Map<string, DumpIssue>> {
  const records = new Map<string, DumpIssue>();
  const scratch: string[] = [];
  try {
    for (const path of paths) {
      let dir = path;
      if (!statSync(path).isDirectory()) {
        dir = mkdtempSync(join(process.env.TMPDIR ?? tmpdir(), "legion-history-"));
        scratch.push(dir);
        await $`tar xzf ${path} -C ${dir}`.quiet();
      }
      for (const name of readdirSync(join(dir, "issues"))) {
        if (!name.endsWith(".json")) continue;
        const record = JSON.parse(readFileSync(join(dir, "issues", name), "utf8")) as DumpIssue;
        records.set(record.issue.key, record);
      }
    }
  } finally {
    for (const dir of scratch) rmSync(dir, { force: true, recursive: true });
  }
  return records;
}

/** `rows` in slices of `size`, one statement per slice. */
async function inSlices<T>(
  rows: readonly T[],
  size: number,
  write: (slice: T[]) => Promise<number>
) {
  let written = 0;
  for (let start = 0; start < rows.length; start += size) {
    written += await write(rows.slice(start, start + size));
  }
  return written;
}

const db = new SQL(args.dsn);
const records = await readDumps(args.dump);
const local = new Map<string, { closed: boolean }>(
  (
    (await db.unsafe(
      `select key, closed_at is not null as closed from issues where project_key = $1`,
      [project]
    )) as { key: string; closed: boolean }[]
  ).map((row) => [row.key, { closed: row.closed }])
);
const unmatched = [...records.keys()].filter((key) => !local.has(key)).sort();
const matched = [...records.values()].filter((record) => local.has(record.issue.key));

const events = matched.flatMap((record) =>
  record.events.map((event) => ({
    issue_key: record.issue.key,
    seq: event.seq,
    type: event.type,
    actor: event.actor,
    payload: event.payload,
    created_at: event.created_at,
  }))
);
const eventsWritten = await inSlices(events, 2000, async (slice) => {
  const result = await db.unsafe(
    `insert into events (issue_key, seq, type, actor, payload, notify, published_at, created_at)
     select e.issue_key, e.seq, e.type, e.actor, e.payload, false, e.created_at, e.created_at
     from jsonb_to_recordset($1::text::jsonb)
       as e(issue_key text, seq int, type text, actor jsonb, payload jsonb, created_at timestamptz)
     on conflict do nothing`,
    [JSON.stringify(slice)]
  );
  return result.count;
});

const handoffs = matched.flatMap((record) =>
  record.handoffs.map((handoff) => ({ ...handoff, issue_key: record.issue.key }))
);
const handoffsWritten = await inSlices(handoffs, 2000, async (slice) => {
  const result = await db.unsafe(
    `insert into handoffs (id, issue_key, phase, version, body, author, created_at)
     select h.id, h.issue_key, h.phase, h.version, '', h.author, h.created_at
     from jsonb_to_recordset($1::text::jsonb)
       as h(id uuid, issue_key text, phase text, version int, author jsonb, created_at timestamptz)
     on conflict do nothing`,
    [JSON.stringify(slice)]
  );
  return result.count;
});

// One ask at a time: the asks table holds each row to checks (an approval's documents, the
// state's answer or resolution) that one malformed row should not fail a whole slice on.
let asksWritten = 0;
const askFailures: string[] = [];
for (const record of matched) {
  for (const ask of record.asks) {
    const answer = ask.state === "answered" ? (ask.answer ?? {}) : null;
    const resolution = ask.state === "resolved" ? (ask.resolution ?? { imported: true }) : null;
    try {
      const result = await db.unsafe(
        `insert into asks (id, issue_key, author, question, options, multiple, urgency, state,
           answer, resolution, kind, approval, created_at, edited_at)
         values ($1, $2, $3::text::jsonb, $4, $5::text::jsonb, $6, $7, $8, $9::text::jsonb,
           $10::text::jsonb, $11, $12::text::jsonb, $13, $14)
         on conflict do nothing`,
        [
          ask.id,
          record.issue.key,
          JSON.stringify(ask.author),
          ask.question,
          JSON.stringify(ask.options ?? []),
          ask.multiple ?? false,
          ask.urgency ?? "med",
          ask.state,
          answer === null ? null : JSON.stringify(answer),
          resolution === null ? null : JSON.stringify(resolution),
          ask.kind,
          ask.kind === "approval" ? JSON.stringify(ask.approval) : null,
          ask.created_at,
          ask.edited_at,
        ]
      );
      asksWritten += result.count;
    } catch (error) {
      askFailures.push(`${record.issue.key} ${ask.id}: ${(error as Error).message}`);
    }
  }
}

// Each issue's own sequence moves past its imported events, so a later local write takes a free
// seq, and a matched issue with no local close time takes the dump's.
const closes = matched.map((record) => ({
  key: record.issue.key,
  closed_at: record.issue.closed_at,
  last_seq: Math.max(0, ...record.events.map((event) => event.seq)),
}));
const [{ closed }] = (await db.unsafe(
  `with dump as (
     select * from jsonb_to_recordset($1::text::jsonb) as d(key text, closed_at timestamptz, last_seq int)
   ), updated as (
     update issues set last_seq = greatest(issues.last_seq, dump.last_seq),
       closed_at = coalesce(issues.closed_at, dump.closed_at)
     from dump where issues.key = dump.key
     returning (issues.closed_at is not null) as closed_now, dump.key
   )
   select count(*)::int as closed from updated where closed_now and key = any($2)`,
  [
    JSON.stringify(closes),
    matched
      .filter((r) => !local.get(r.issue.key)?.closed && r.issue.closed_at)
      .map((r) => r.issue.key),
  ]
)) as { closed: number }[];
await db.close();

console.log(
  JSON.stringify(
    {
      project,
      dump_issues: records.size,
      matched: matched.length,
      unmatched: unmatched.length,
      unmatched_keys: unmatched,
      events: { dumped: events.length, written: eventsWritten },
      handoffs: { dumped: handoffs.length, written: handoffsWritten },
      asks: {
        dumped: matched.reduce((n, r) => n + r.asks.length, 0),
        written: asksWritten,
        failed: askFailures,
      },
      close_times_filled: closed,
    },
    null,
    2
  )
);
