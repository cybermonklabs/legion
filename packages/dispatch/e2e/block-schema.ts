/**
 * The block schema as the store holds it, for the two specs that move it under a running server:
 * `e2e/graph-store.e2e.ts` and `e2e/schema-switch.multiprocess.e2e.ts`.
 *
 * A type is data: one `supertag` node per block type and one `attribute` node per attribute it
 * holds, seeded from `packages/envoy/internal/dispatch/pmdoc/schema/blocks.json` by migration
 * 20261109051232, which also installs the trigger that announces a write to either kind. Each
 * server reads those nodes at boot and again at every announcement, and installs what it read
 * (`pmdoc/schemanodes`). So a supertag migration is SQL, and these helpers write it the way that
 * migration does.
 *
 * Which way the version moves is decided by the binary, not by taste: a store schema above the
 * version the binary embeds installs nothing and leaves the process not ready, which is the case
 * `c-schema-holder` reads in Go. A browser run therefore starts the store one version below the
 * binary's own, with the new type absent (`storeSchemaBeforeTheChange`), and the migration moves
 * it up to the binary's own version with the type in place (`applySupertagMigration`): one
 * transaction, as the Go test's own flip is, so no reader sees half a change.
 */
import { readFileSync } from "node:fs";

import type { BlockSchema } from "@legion/contracts";

import { baseUrl } from "./api";
import { type DispatchProcess, pinnedTo } from "./harness-processes";
import { sql } from "./psql";

/** The block type the migration these specs apply adds. It is no type of the shipped schema, so a
 *  document holding one is refused by a server that has not installed the new schema - which is
 *  what makes a stale process readable from outside it. */
export const ADDED_TYPE = "claim";

/** A document holding a block of the added type, as markdown. */
export function documentWithAddedType(line: string): string {
  return `# Notes\n\n:::${ADDED_TYPE}{#c1}\n${line}\n:::\n`;
}

/** The schema version this build embeds, read from the file the migration seeded the store from:
 *  the highest version the binary can install, and the version the store ships at. */
export const builtSchemaVersion: number = (
  JSON.parse(
    readFileSync(
      new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
      "utf-8"
    )
  ) as BlockSchema
).version;

/** The version the store holds before the migration: one below the binary's own. */
export const priorSchemaVersion = builtSchemaVersion - 1;

/** The schema one process serves now, read through `GET /api/v1/schema/blocks`, which answers the
 *  installed value. With `process` the read is pinned to that process of the two-process lane. */
export async function installedSchema(process?: DispatchProcess): Promise<BlockSchema> {
  const response = await fetch(new URL("/api/v1/schema/blocks", baseUrl), {
    headers: process === undefined ? {} : pinnedTo(process),
  });
  if (!response.ok) {
    throw new Error(`GET /api/v1/schema/blocks answered ${response.status}`);
  }
  return (await response.json()) as BlockSchema;
}

/** The version one process serves now. */
export async function installedVersion(process?: DispatchProcess): Promise<number> {
  return (await installedSchema(process)).version;
}

/** Whether the schema one process serves declares the added type. */
export async function servesAddedType(process?: DispatchProcess): Promise<boolean> {
  return (await installedSchema(process)).types.some((type) => type.name === ADDED_TYPE);
}

/** The statements that take the added type out of the store and set every block type's version,
 *  in one transaction: writing the nodes is what announces the change, and two writes outside a
 *  transaction announce it twice and leave a window in which the type is gone and the version
 *  still the old one. The version is written only where it moves, so a restore that changes
 *  nothing announces nothing and no process reinstalls a schema it already serves. A caller runs
 *  these and then waits for the processes to install what they leave. */
function storeSchemaStatements(version: number): string[] {
  return [
    "begin",
    `delete from graph_tuples where from_kind = 'attribute' and from_id like '${ADDED_TYPE}/%'`,
    `delete from graph_nodes where (kind = 'supertag' and id = '${ADDED_TYPE}')
       or (kind = 'attribute' and id like '${ADDED_TYPE}/%')`,
    `update graph_nodes set attributes = jsonb_set(attributes, '{version}', to_jsonb(${version}::int))
     where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false)
       and attributes->>'version' is distinct from '${version}'`,
    "commit",
  ];
}

/** Puts the store at the schema these specs start from: the version below the binary's own, with
 *  the added type absent. */
export async function storeSchemaBeforeTheChange(): Promise<void> {
  await sql(...storeSchemaStatements(priorSchemaVersion));
}

/** Puts the store back at the schema it ships with, so the specs that run after these read the
 *  server every other spec has always read. */
export async function restoreStoreSchema(): Promise<void> {
  await sql(...storeSchemaStatements(builtSchemaVersion));
}

/** The writer a migration's rows carry, as 20261109051232's seed carries one of its own. */
const migrationWriter = `'{"kind": "system", "id": "e2e-supertag-migration"}'::jsonb`;

/**
 * The supertag migration these specs apply: it adds the type and moves every block type's version
 * to the binary's own, in one transaction, so the commit is the one moment the store's schema
 * changes and a process reads either the whole of it or none. With `stopListenerOf` the listener
 * connection of that process is terminated first, in the same psql session and before the commit,
 * so that process is not listening when the notification is sent and stays on the schema it has
 * until its hub redials (`pglisten.Hub`, whose redial is why the delay ends on its own).
 */
export async function applySupertagMigration(
  options: { readonly stopListenerOf?: string } = {}
): Promise<void> {
  const stop =
    options.stopListenerOf === undefined
      ? []
      : [
          `select pg_terminate_backend(pid) from pg_stat_activity
            where datname = current_database() and application_name = '${options.stopListenerOf}'`,
        ];
  await sql(
    ...stop,
    "begin",
    `insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
     values
       ('supertag', '${ADDED_TYPE}', 'supertag', 'project', 'live',
         jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
           'version', ${builtSchemaVersion}::int, 'position', 99),
         now(), ${migrationWriter}, 0),
       ('attribute', '${ADDED_TYPE}/supports', 'attribute', 'project', 'live',
         jsonb_build_object('name', 'supports', 'store_name', 'supports', 'position', 0,
           'kind', 'block[]', 'targets', jsonb_build_array('requirement')),
         now(), ${migrationWriter}, 0)
     on conflict (kind, id) do update set state = 'live', attributes = excluded.attributes`,
    `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
     select 'attribute', '${ADDED_TYPE}/supports', 'attribute_of', 'supertag', '${ADDED_TYPE}',
            now(), ${migrationWriter}, 0, null, true
     where not exists (
       select 1 from graph_tuples
       where from_kind = 'attribute' and from_id = '${ADDED_TYPE}/supports' and kind = 'attribute_of'
     )`,
    `update graph_nodes set attributes = jsonb_set(attributes, '{version}', to_jsonb(${builtSchemaVersion}::int))
     where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false)`,
    "commit"
  );
}

/** How a server process names its one shared listener connection in `pg_stat_activity`:
 *  `docs.ListenerApplicationPrefix` and the process's own id (`docs.Service.ProcessID`). */
export const listenerApplicationPrefix = "dispatch-listener ";

/** The listener connection of every server process on this database, oldest connection first. The
 *  harness starts its processes one after another, each serving before the next begins, so the
 *  first row is the first process started. */
export async function listeners(): Promise<string[]> {
  const rows = await sql(
    `select application_name from pg_stat_activity
     where datname = current_database() and application_name like '${listenerApplicationPrefix}%'
     order by backend_start, application_name`
  );
  return rows === "" ? [] : rows.split("\n");
}

/** Each live server process with the time it started, from the registry every process writes
 *  itself into at boot (`server_processes`, `internal/dispatch/processes`). A process that
 *  restarted writes a row with a new instance id and a later start, so comparing this across a
 *  run is how a spec proves no process restarted. */
export async function processStartTimes(): Promise<string> {
  return sql(
    `select instance::text || ' started ' || started_at from server_processes order by started_at, instance`
  );
}
