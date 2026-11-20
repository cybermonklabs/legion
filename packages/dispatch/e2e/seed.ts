import { resetFakeEnvoy } from "./agents";
import { forgetSessions, quiesceDocuments, resetLogins } from "./api";
import { resetFakeBroker } from "./fake-broker-helpers";
import { resetFakeEmbedder } from "./fake-embedder-helpers";
import { resetFakeGithub } from "./fake-github-helpers";
import { measured } from "./measure";
import { sql } from "./psql";

// The dashboard tables (dashboard_builds, dashboard_assets, dashboard_build_assets, migration 0136)
// are not on this list, and no foreign key ties them to a table on it, so the deletes below never
// reach them: every server process publishes its build's assets there once, at boot, and in
// two-process mode each answers the other build's chunks from there. A reset between scenarios
// must not delete what both processes published when they started. The registry of live server
// processes (server_processes, migration 0135) stays off it too: a process registers once, at
// boot, and GET /api/v1/dashboard/builds reads the builds the live ones serve from it.
const tables = [
  "agent_tokens",
  "repo_projects",
  "architecture_sources",
  "legion_current",
  "legion_observations",
  "legion_reads",
  "legion_settings",
  "delivery_settings",
  "delivery_changes",
  "delivery_session_group_members",
  "delivery_session_groups",
  "delivery_run_jobs",
  "delivery_runs",
  "delivery_pull_requests",
  // pull_request_heads and check_results are reached by the cascade below (both carry a
  // foreign key into issues); branch_commits carries none, so it is named here.
  "branch_commits",
  "user_issue_state",
  "user_agent_state",
  // The agents a person pinned (migration 20261106215223), a table of its own that no foreign
  // key reaches: a pin a row leaves behind would order the next row's Agents list and put its
  // sessions in the next row's sidebar.
  "user_agent_pin",
  "user_agent_read",
  "user_agent_reply_read",
  "user_agent_thread_dismissed",
  "user_threads",
  "events",
  "refs",
  "messages",
  "broadcasts",
  "comment_fallback_copies",
  "comments",
  "asks",
  "doc_checkpoints",
  "doc_snapshots",
  "doc_updates",
  "artifact_versions",
  "artifacts",
  "issue_external_links",
  "issues",
  "projects",
  "people",
  // The cross-project priority and its tuple log (INFRA-2947, migration
  // 20261112024320_priority_nodes). No foreign key reaches either table, so the closure below
  // does not find them and they are named here: a priority a row wrote would otherwise stand
  // in the next row's rail count, and its id would already be taken, since a priority is ended
  // by its state and its node row stays.
  "priority_tuples",
  "priority_nodes",
  // The groups an access list names (migration 20261116015736_project_access). No foreign key
  // reaches them either - a group belongs to no project, and its owner is an email rather than a
  // people row - so the closure does not find them: a group one row made would stand in the next
  // row's Settings and its name would be taken. Its members follow it through their own key.
  "access_groups",
  // The projects a session acted in (the same migration), which names its project by key with no
  // foreign key, so a session's write takes no lock on projects: the closure does not reach it,
  // and a row one row wrote would make the next row's session one of a project's agents.
  "session_projects",
  // The step records Legion's daemon writes of an issue, and their tuple log (the
  // `workflow_nodes` migration). No foreign key reaches either table, so the closure below does
  // not find them: a step a row wrote would otherwise stand in the next row's Steps tab, under
  // the issue key the next row's issue takes again.
  "workflow_tuples",
  "workflow_nodes",
  // The passkeys people from outside enrolled (migration 20261120165538_outside_passkeys), which
  // name their person by email with no foreign key to people, so the closure does not reach them:
  // a passkey one row enrolled would stand on the next row's Access tab and sign in there. The
  // owner's links of the same migration (outside_invitations) carry a key into projects, which
  // the closure follows.
  "passkeys",
  "user_sessions",
];

// The node and tuple store (migration 20261109013400), which the closure above never reaches:
// it walks foreign keys from a named table to the tables that reference it, and the key runs the
// other way - `comments`, `document_blocks` and `block_refs` carry it into `graph_nodes` - so
// naming them empties the rows and leaves the nodes those rows project. A reseeded database then
// holds nodes whose rows are gone, which `envoy-dispatch census --store` reads as "a live node no
// row projects". `graph_tuples` carries no foreign key at all, and `graph_erasures` is the record
// of an erase, which a scenario of its own writes. So the three are emptied here instead, by a
// delete of their own, and locked beside the closure's tables so the reset still holds every
// table it empties.
//
// The schema's own nodes stay: migration 20261109051232 seeds one `supertag` node per block type
// and one `attribute` node per attribute, with their base-attribute and `attribute_of` tuples,
// and nothing writes them again until a migration runs. They are the block schema the server
// installs (pmdoc/schemanodes) and the targets `block_pointer_targets` reads for `graph_edges`,
// so a reset that took them would leave the instance with no block type at all. `source_event`
// tells them apart for nothing: the kind does, and that is what these predicates keep.
const storeDeletes: ReadonlyArray<{ table: string; keep?: string }> = [
  { table: "graph_erasures" },
  { table: "graph_tuples", keep: "from_kind IN ('supertag', 'attribute')" },
  { table: "graph_nodes", keep: "kind IN ('supertag', 'attribute')" },
];

/** `value` as a SQL string literal, with every quote of its own doubled. psql interpolates no
 *  variable inside a quoted literal, so text reaches Postgres as written. */
export function sqlLiteral(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

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
// timers. So before the delete, the reset takes ACCESS EXCLUSIVE on every table it can empty: the
// tables it names, and every table whose foreign keys reach them, which a cascading delete empties
// too. It never waits while it holds one of those locks. Each try takes them all with NOWAIT; when
// one is held, the try lets go of every lock it took, and the next try first waits for that one
// table while it holds none, so the wait cannot close a cycle. Once it holds them all, nothing
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
        locked regclass[];
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
        deadline timestamptz := clock_timestamp() + interval '5 seconds';
      BEGIN
        -- The closure a TRUNCATE ... CASCADE of these tables would empty, which is the closure
        -- the deletes below reach. packages/envoy/internal/pgmigrate/touched.go
        -- (reachedByForeignKeys) walks the same closure for a migration's truncate.
        WITH RECURSIVE truncated(rel) AS (
          SELECT unnest(ARRAY[${tables.map(sqlLiteral).join(", ")}]::regclass[])
          UNION
          SELECT c.conrelid::regclass
            FROM pg_constraint c JOIN truncated ON c.confrelid = truncated.rel
            WHERE c.contype = 'f'
        )
        SELECT array_agg(rel ORDER BY rel::text) INTO targets FROM truncated;
        -- Every table this block empties, which is the closure plus the store's own three: the
        -- locks cover what the scoped deletes below touch as well, so nothing writes a node or a
        -- tuple between the projection's delete and theirs.
        locked := targets || ARRAY[${storeDeletes.map((entry) => sqlLiteral(entry.table)).join(", ")}]::regclass[];
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
            FOREACH target IN ARRAY locked LOOP
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
        -- The reset is the one writer that removes rows the account binding guards, which the
        -- database refuses to anything else (migrations 20261117175133 and 20261118041916): a
        -- people row recording a Google account (people_recorded_account_kept) and a
        -- user_sessions row (user_sessions_binding_columns). It turns those two triggers off for
        -- this transaction alone, under the locks it holds, and back on before it inserts the
        -- logins below. A database migrated by an earlier head of cybermonklabs/legion#4199, whose
        -- migration 20261117175133 changed in place before it merged, can lack the first; the
        -- reset then stops and says how to start over, with a real broker's database beside this
        -- one as Dispatch's runbook says, rather than naming a missing trigger. (The second is
        -- 20261118041916's, which the server applied before it served this reset.)
        IF NOT EXISTS (SELECT 1 FROM pg_trigger
                        WHERE tgrelid = 'people'::regclass AND tgname = 'people_recorded_account_kept') THEN
          RAISE EXCEPTION 'this database lacks people_recorded_account_kept, so an earlier head of cybermonklabs/legion#4199 migrated it (migration 20261117175133 changed in place before that pull request merged): drop the database DATABASE_URL names, create it empty, and let the server migrate it, with a real secrets broker''s database (BROKER_DATABASE_URL, when DISPATCH_E2E_AGENT_SECRETS_URL names one) beside it, as Dispatch''s runbook says (running-dispatch.md, "Signing people in")';
        END IF;
        ALTER TABLE people DISABLE TRIGGER people_recorded_account_kept;
        ALTER TABLE user_sessions DISABLE TRIGGER user_sessions_binding_columns;
        -- Unlike TRUNCATE, a DELETE fires the row triggers, and the delivery tables' log their
        -- own deletions into delivery_changes. So a pass that emptied a table whose deletion
        -- fires one (pg_trigger's tgtype bit 8, enabled) probes again; the log table itself has
        -- no such trigger, so the second pass ends it. Nothing else in this schema deletes into
        -- another table, so a reset of rows no delivery spec wrote pays one pass.
        SELECT coalesce(array_agg(DISTINCT t.tgrelid::regclass), '{}') INTO logging
          FROM pg_trigger t
          WHERE t.tgrelid = ANY (targets::oid[]) AND NOT t.tgisinternal AND (t.tgtype & 8) <> 0
            AND t.tgenabled <> 'D';
        LOOP
          -- Almost every row dirties a handful of the closure's tables, and emptying the ones that
          -- hold no rows is most of what a TRUNCATE of all of them cost. Each table is probed
          -- instead, and only the ones holding rows are emptied.
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
        -- The store, after the projections it holds: a node may go only once the rows whose
        -- foreign key names it are gone, which the loop above did. Each delete keeps the rows
        -- the schema migration seeded and takes every row a scenario wrote.
${storeDeletes
  .map(
    (entry) =>
      `        DELETE FROM ${entry.table}${entry.keep === undefined ? "" : ` WHERE NOT (${entry.keep})`};`
  )
  .join("\n")}
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
        ALTER TABLE people ENABLE TRIGGER people_recorded_account_kept;
        ALTER TABLE user_sessions ENABLE TRIGGER user_sessions_binding_columns;
        -- The people the scenarios act as, as if each had signed in: the only assignable names.
        INSERT INTO people (email) VALUES ${resetLogins.map((login) => `(${sqlLiteral(login)})`).join(", ")};
        -- And the session a sign-in of each would have created (PgSessionStore.EnsureSession
        -- inserts the row, whose generation column defaults to 0; these rows record no account, so
        -- nothing advances it), at that same generation 0. The delete above took every \`user_sessions\`
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
