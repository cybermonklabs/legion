import { resetFakeEnvoy } from "./agents";
import { forgetSessions, quiesceDocuments, resetLogins } from "./api";
import { resetFakeBroker } from "./fake-broker-helpers";
import { resetFakeEmbedder } from "./fake-embedder-helpers";
import { resetFakeGithub } from "./fake-github-helpers";
import { measured } from "./measure";
import { sql } from "./psql";

// What the reset empties is read from the schema: every ordinary table of the `public` schema
// except the ones `keptTables` names. A table a later migration adds is emptied between
// scenarios with nobody writing it down here.
//
// Nothing has to be named here for the reset to reach a table, and the control that holds this
// is `e2e/reset-unnamed-table.e2e.ts`: one of its rows writes into a table nothing here names,
// and the row after it reads that table back empty.

/** The tables the reset must not empty, each beside the reason it stays.
 *
 *  Everything else in `public` is emptied, so an entry here is a judgement a later reader must
 *  be able to check: it says what happens in the world when the reset takes this table's rows,
 *  never that keeping them is safer.
 *
 *  Four of the cursors, horizons and backfill markers a sweep writes, and no scenario seeds,
 *  are entries here, each for what its own absence does to the code that reads it. The comment
 *  under this list says what the reset does to the others. */
const keptTables: readonly { table: string; why: string }[] = [
  {
    table: "schema_migrations",
    why: "Which migrations this database has run. A server process applies the migrations at boot and runs the ones this table does not name, so a reset that emptied it would make the next process to start run all of them again over the objects they already created, and fail on the first.",
  },
  {
    table: "dashboard_builds",
    why: "The web build each server process serves, published here once, at boot. In two-process mode each process answers the other build's chunk requests from these rows, and GET /api/v1/dashboard/builds reads the builds the live processes serve. Nothing publishes a build again while a process runs, so a reset that emptied these rows would leave both processes serving a build the database no longer holds.",
  },
  {
    table: "dashboard_assets",
    why: "The files of those builds, published beside them at boot and read whenever a page asks for a chunk of the other process's build.",
  },
  {
    table: "dashboard_build_assets",
    why: "Which file belongs to which build: the same publication at boot, and the join a chunk request reads through.",
  },
  {
    table: "server_processes",
    why: "The registry of the processes serving now, one heartbeated row each. A process whose row has gone asks the registry to admit it again (processes.Registration.renew), and GET /api/v1/dashboard/builds reads the builds of the live rows, so a reset that emptied it would make every process re-admit itself mid-scenario and leave that read answering nothing until they had.",
  },
  {
    table: "duty_holds",
    why: "The leases the running processes hold now. A renewal that reaches no row tells its holder that another process took the lease, and the holder stops the loop that lease guards (duty/hold.go, TryHold). A stack of this suite takes no lease while the fake Envoy stands in for NATS - no row in 40 samples through a whole run - so emptying this table costs nothing on almost every reset and a stopped sweep on the one where a process does hold one.",
  },
  // The four rows below are a migration's own, inserted with no data behind them, and each is
  // read as exactly one row. The suite measured what emptying them costs: with
  // document_search_backfill emptied, the next server to boot logged "read the document_search
  // fill's progress: no rows in result set" and exited 1, and the harness refused to start at
  // all (2026-11-12, this pod). The other three are read the same way. No migration inserts a
  // row into any other table of this schema except from the rows already there, which the reset
  // empties beside them.
  {
    table: "document_search_backfill",
    why: "The one row that says how far the fill of document_search for documents stored before that release has got (migration 20261101182501). A process reads `select done from document_search_backfill` at boot, over one row, and a boot that finds none logs the read's error and exits 1: measured, the harness could then start no stack at all.",
  },
  {
    table: "embeddings_project_key_backfill",
    why: "The same shape for the project key each embedding carries (migration 20261109120100): the fill reads `select kind, id from embeddings_project_key_backfill for update`, over the one row the migration inserted, and nothing inserts it again.",
  },
  {
    table: "suggestion_outcome_cursor",
    why: "The one row holding the last event the suggestion sweep read (migration 0154), which that sweep reads as `select event_id from suggestion_outcome_cursor for update`. Emptying it leaves the sweep reading no row at all. What the row then holds is an event id of a scenario that has ended, and the reset restarts the events sequence under it, so the sweep reads no event of the next scenario either - a leak this reset cannot close by emptying a table, and the one the suite has always run with.",
  },
  {
    table: "embeddings_rate_limit",
    why: "The one row the embedder calls draw their tokens from (migration 0078, 200,000 at the start). The draw is an update of that row returning what it took, so with the row gone it takes nothing and every embedding waits for a refill that no row records.",
  },
];

// Beside the rows a scenario itself wrote, the reset empties the tables below, and what
// emptying each one does is the reason it is safe to empty.
//
// Four of the eight cursors, horizons, queues and markers of this schema are emptied:
//
// - event_horizon, the highest event id a reader may read. The row names an event by its id and
//   by that event's created_at, and both statements that touch it skip a row whose event is gone
//   (events/horizon.go), so a row kept past a reset of the event log already answered nothing.
// - duty_passes, the period each leased sweep last ran a pass in. Emptied, the next tick of a
//   sweep runs its pass rather than finding the period served, which is one more pass of work
//   the scenarios pay for on their own timers anyway.
// - embeddings, each keyed by the id of a document or a comment the reset deletes. A kept
//   embedding is the text of a scenario that has ended, and the rows that read search seed what
//   they read after the reset.
// - search_term_queue, the terms owed indexing, each naming an issue, an artifact or an item the
//   reset deletes.
//
// The other four are the singleton rows above, which the suite refused: emptying them leaves a
// reader that expects exactly one row with none.
//
// The rest of what the reset empties beside a scenario's own rows is per-scenario data or a
// cache of it - document_search, embedding_window_states, doc_compaction_owed,
// refs_grammar_owed, block_grammar_owed, request_keys, going_requests and going_events,
// delivery_run_warnings, delivery_reconcile_progress, ci_reconcile_passes,
// reference_excerpt_backoff, dpop_proofs, github_rate_limits, route_roster and
// server_build_announcements - each keyed by a row the reset deletes, or written again by the
// process that reads it. Eleven of them held no row in any of 40 samples taken through one run
// of the chromium project, on this pod on 2026-11-12: block_grammar_owed,
// ci_reconcile_passes, delivery_reconcile_progress, dpop_proofs, github_rate_limits, going_events,
// going_requests, reference_excerpt_backoff, route_roster, server_build_announcements and
// duty_holds, which is kept above for what one row of it would mean rather than for the rows it
// holds here.

function sqlLiteral(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

// The kept tables as one SQL list. The reset interpolates it twice, into the check that every
// kept name is a table of this schema and into the read that leaves those tables out of the
// targets, and one list is what makes those two the same set.
const keptTableList = keptTables.map((kept) => sqlLiteral(kept.table)).join(", ");

export async function insertExternalLink(issueKey: string, url: string): Promise<void> {
  await sql(
    `INSERT INTO issue_external_links (issue_key, url, kind) VALUES (${sqlLiteral(issueKey)}, ${sqlLiteral(url)}, 'url')`
  );
}

export async function setEventCreatedAt(eventId: number, iso: string): Promise<void> {
  await sql(
    `UPDATE events SET created_at = ${sqlLiteral(iso)}::timestamptz WHERE id = ${Number(eventId)}`
  );
}

/** Dates a seeded comment or message: the server stamps `created_at` itself, so two rows in one
 *  second or one millisecond with chosen fractions can only be fixtured on the row. */
export async function setCreatedAt(
  table: "comments" | "messages",
  id: string,
  iso: string
): Promise<void> {
  await sql(
    `UPDATE ${table} SET created_at = ${sqlLiteral(iso)}::timestamptz WHERE id = ${sqlLiteral(id)}::uuid`
  );
}

/** Stamps a verified service token's subject on a seeded comment's author. The API seeder posts
 *  its actor under the shared token and the server drops a body-supplied `service`, so the only
 *  way to fixture a service-authored write is the `comments.author` jsonb itself. */
export async function setCommentAuthorService(commentId: string, service: string): Promise<void> {
  await sql(
    `UPDATE comments SET author = author || jsonb_build_object('service', ${sqlLiteral(service)}) WHERE id = ${sqlLiteral(commentId)}::uuid`
  );
}

/** Marks a newly created fixture issue as pre-creator metadata. */
export async function clearIssueCreator(issueKey: string): Promise<void> {
  await sql(`UPDATE issues SET created_by = 'null'::jsonb WHERE key = ${sqlLiteral(issueKey)}`);
}

// The reset's delete locks its tables one at a time, in its own order, waiting for each while it
// holds the ones before it. A transaction that reads two of those tables in another order can
// close a cycle with it (the Inbox reads asks before comments, the sidebar issues before
// user_issue_state), and PostgreSQL breaks the cycle by aborting one side: the reset, or a server
// request or sweep. Such a transaction can start at any moment: a page an earlier scenario left
// open refetches whenever the live stream tells it to, and the server runs its own sweeps on
// timers. So before the delete, the reset takes ACCESS EXCLUSIVE on every table it empties: each
// ordinary table of the `public` schema that `keptTables` does not name, which is 104 of the 114
// tables this schema's migrations create. It never waits while it holds one of those locks. Each
// try takes them all with NOWAIT. When one is held, the try lets go of every lock it took, and
// the next try first waits for that one table while it holds none, so the wait cannot close a
// cycle. Once it holds them all, nothing
// else touches those tables until the block commits; the reset's other locks are on the tables'
// own sequences, which only an insert into one of those tables advances. Each wait is capped at
// the time left of 5 s, so a table still held 5 s after the step began fails the reset then,
// naming that table's holders.

// One statement: a DO block is a transaction of its own, so the inserts that end it are the ones
// the delete's own transaction makes, and the reset pays psql one round trip rather than the five
// a BEGIN, the block, each insert and a COMMIT paid.
async function resetDatabaseOnce(): Promise<void> {
  await sql(
    `DO $$
      DECLARE
        targets regclass[];
        target regclass;
        logging regclass[];
        refilled boolean;
        emptied regclass[] := '{}';
        passes int := 0;
        deletes text[];
        sequence_name text;
        holds_rows boolean;
        contended regclass;
        holders text;
        missing text[];
        deadline timestamptz := clock_timestamp() + interval '5 seconds';
      BEGIN
        -- A kept name that no table of this schema carries excuses nothing, and the migration
        -- that renamed or dropped the table would leave the name here unread. The list is
        -- checked against the schema instead, so such a name fails the reset and names itself.
        SELECT array_agg(name ORDER BY name) INTO missing
          FROM unnest(ARRAY[${keptTableList}]::text[])
            AS kept(name)
          WHERE to_regclass('public.' || quote_ident(name)) IS NULL;
        IF missing IS NOT NULL THEN
          RAISE EXCEPTION 'the reset keeps tables this schema has not got: %', missing;
        END IF;
        -- Every ordinary table of the public schema but those, in a fixed order, which is the
        -- order the locks below are taken in. A table a later migration adds is a target the day
        -- it exists, with nobody naming it here.
        SELECT array_agg(c.oid::regclass ORDER BY c.relname) INTO targets
          FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
          WHERE n.nspname = 'public' AND c.relkind = 'r'
            AND c.relname <> ALL (ARRAY[${keptTableList}]::text[]);
        LOOP
          -- A failed try rolls back this block, releasing every lock it took.
          BEGIN
            IF contended IS NOT NULL THEN
              -- PostgreSQL checks a wait for a deadlock after deadlock_timeout (1 s by default);
              -- this wait holds none of the tables' locks, so no cycle can run through it.
              PERFORM set_config('lock_timeout',
                greatest(1, ceil(1000 * extract(epoch FROM deadline - clock_timestamp())))::int || 'ms',
                true);
              EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE', contended);
            END IF;
            FOREACH target IN ARRAY targets LOOP
              EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE NOWAIT', target);
            END LOOP;
            EXIT;
          EXCEPTION WHEN lock_not_available THEN
            contended := target;
          END;
          IF clock_timestamp() >= deadline THEN
            -- Every lock mode conflicts with ACCESS EXCLUSIVE, so each granted lock on the table
            -- the last try waited for is one that blocked it.
            SELECT string_agg(DISTINCT format('%s (%s, %s)',
                     left(regexp_replace(a.query, '\\s+', ' ', 'g'), 120), l.mode, a.state), '; ')
              INTO holders
              FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
              WHERE l.granted AND l.pid <> pg_backend_pid() AND l.relation = contended;
            RAISE EXCEPTION 'could not lock % to truncate it within 5 s; held by: %', contended,
              coalesce(holders, 'a transaction that ended just before this report');
          END IF;
        END LOOP;
        -- The waits above are the only ones lock_timeout bounds.
        PERFORM set_config('lock_timeout', '0', true);
        -- Unlike TRUNCATE, a DELETE fires the row triggers, and the delivery tables' log their
        -- own deletions into delivery_changes. So a pass that emptied a table whose deletion
        -- fires one (pg_trigger's tgtype bit 8) probes again; the log table itself has no such
        -- trigger, so the second pass ends it. Nothing else in this schema deletes into another
        -- table, so a reset of rows no delivery spec wrote pays one pass.
        SELECT coalesce(array_agg(DISTINCT t.tgrelid::regclass), '{}') INTO logging
          FROM pg_trigger t
          WHERE t.tgrelid = ANY (targets::oid[]) AND NOT t.tgisinternal AND (t.tgtype & 8) <> 0;
        LOOP
          -- Almost every scenario dirties a handful of those tables, and emptying the ones
          -- that hold no rows is most of what the old TRUNCATE of all of them cost. Each table is
          -- probed instead, and only the ones holding rows are emptied.
          deletes := '{}';
          refilled := false;
          FOREACH target IN ARRAY targets LOOP
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %s)', target) INTO holds_rows;
            IF holds_rows THEN
              emptied := emptied || target;
              deletes := deletes || format('d%s AS (DELETE FROM %s)', cardinality(deletes), target);
              refilled := refilled OR target = ANY (logging);
            END IF;
          END LOOP;
          EXIT WHEN cardinality(deletes) = 0;
          -- One statement, because this schema's foreign keys that are neither CASCADE nor SET
          -- NULL are NO ACTION (none is RESTRICT), and a NO ACTION reference is checked at the
          -- end of the statement: every delete is done by then, so no order among them is needed
          -- and a referenced row emptied after its referrers is still fine. Every CTE of a
          -- data-modifying WITH runs, whether the main query reads it or not.
          EXECUTE 'WITH ' || array_to_string(deletes, ', ') || ' SELECT 1';
          EXIT WHEN NOT refilled;
          passes := passes + 1;
          IF passes > 3 THEN
            RAISE EXCEPTION 'tables still held rows after % delete passes: %', passes, deletes;
          END IF;
        END LOOP;
        -- What RESTART IDENTITY did, for the tables that were emptied: the sequences their own
        -- columns own (pg_depend's 'a' for a serial, 'i' for an identity column). A DELETE leaves
        -- them where they stood, and the next insert would then collide with a row a trigger
        -- logged under a seq this reset gave back (delivery_changes), so this is not optional.
        FOR sequence_name IN
          SELECT DISTINCT s.oid::regclass::text
            FROM pg_class s
            JOIN pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = s.oid
              AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')
            WHERE s.relkind = 'S' AND d.refobjid = ANY (emptied::oid[])
        LOOP
          EXECUTE format('ALTER SEQUENCE %s RESTART', sequence_name);
        END LOOP;
        -- The people the scenarios act as, as if each had signed in: the only assignable names.
        INSERT INTO people (email) VALUES ${resetLogins.map((login) => `(${sqlLiteral(login)})`).join(", ")};
        -- And the session a sign-in of each would have created (PgSessionStore.EnsureSession
        -- inserts the row, whose generation column defaults to 0, and only RevokeSessions
        -- advances it), at that same generation 0. The delete above took every \`user_sessions\`
        -- row with it, and the server admits a cookie only at its login's current generation
        -- (identity.CookieIdentity.Login), so without this row a cookie minted before the reset
        -- would meet none and be refused; with it, the cookie e2e/api.ts kept for alice or bob
        -- authenticates again and this scenario pays no sign-in for them (forgetSessions).
        INSERT INTO user_sessions (login, generation) VALUES ${resetLogins
          .map((login) => `(${sqlLiteral(login)}, 0)`)
          .join(", ")};
      END
    $$`
  );
}

/**
 * Empties every table, after the server has closed every live document and finished the
 * settlements in flight, so the documents the previous scenario had open finish writing before
 * the delete rather than failing against the emptied tables after it. The delete cannot
 * deadlock with whatever else still reaches the database (the comment above resetDatabaseOnce).
 * It also clears the fake Envoy, whose
 * process-local subscriptions outlive an emptied database and otherwise match recycled issue keys,
 * the fake broker, whose seeded credential requests would otherwise reach every later row's
 * Inbox, the fake GitHub, whose installations would otherwise let a later row's reconcile
 * succeed against repositories it never seeded, and the fake embedder, whose counted calls and
 * seeded markers belong to the row that made them; those resets wait on nothing in the database,
 * so they run beside the quiesce and delete, which keep their order. All settle before this
 * returns, a failed one included, so a reset that fails never leaves another running into the
 * next test's.
 * The delete takes `user_sessions` with it and the inserts above put alice's and bob's rows back
 * at generation 0, so their cookies authenticate again and e2e/api.ts keeps those two
 * (`forgetSessions`, which also forgets every other login's and every browser sign-in since the
 * last reset); a context signing in as one of them carries the kept cookie instead of minting
 * (e2e/users.ts).
 *
 * The two sides are the timing fixture's parts `reset-sql` and `reset-fakes` (e2e/measure.ts):
 * the fakes are one part, settled together so one failing still lets the others finish.
 */
export async function resetDatabase(): Promise<void> {
  const resets = await Promise.allSettled([
    measured("reset-sql", () => quiesceDocuments().then(resetDatabaseOnce)),
    measured("reset-fakes", async () => {
      const fakes = await Promise.allSettled([
        resetFakeEnvoy(),
        resetFakeBroker(),
        resetFakeGithub(),
        resetFakeEmbedder(),
      ]);
      for (const fake of fakes) if (fake.status === "rejected") throw fake.reason;
    }),
  ]);
  for (const reset of resets) if (reset.status === "rejected") throw reset.reason;
  forgetSessions();
}
