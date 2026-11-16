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
 * The migration is one of the release after this build's (`applySupertagMigration`): in one
 * transaction it raises the store to the version one past the binary's own and adds two typed
 * blocks, `claim`, which declares no since and so every build reads it, and `future`, which that
 * newer version introduces. A process of this build installs a store above its own version at its
 * own version (`pmdoc.Install`): it takes `claim`, which is what moves the schema it serves and
 * closes its document sockets, leaves `future` out, and keeps serving. The store starts at the
 * version it ships at, the binary's own, with neither type (`restoreStoreSchema`): a page of this
 * build presents that version and the highest version that reads what it creates, and a store
 * below it would leave the page read-only before anything moved.
 */
import { readFileSync } from "node:fs";

import type { BlockSchema } from "@legion/contracts";

import { baseUrl, e2eAgentToken } from "./api";
import { type DispatchProcess, pinnedTo } from "./harness-processes";
import { sql } from "./psql";

/** The block type the migration these specs apply adds for every build: it is no type of the
 *  shipped schema, so a document holding one is refused by a server that has not installed the
 *  new schema - which is what makes a stale process readable from outside it. */
export const ADDED_TYPE = "claim";

/** The block type the migration adds at the newer version, which a process of this build does not
 *  install. */
export const NEWER_TYPE = "future";

/** A document holding a block of the added type, as markdown. */
export function documentWithAddedType(line: string): string {
  return `# Notes\n\n:::${ADDED_TYPE}{#c1}\n${line}\n:::\n`;
}

/** The schema version this build embeds, read from the file the migration seeded the store from:
 *  the version it compiles into its pages, installs a store at, and the store ships at. */
export const builtSchemaVersion: number = (
  JSON.parse(
    readFileSync(
      new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
      "utf-8"
    )
  ) as BlockSchema
).version;

/** The version the migration raises the store to: one past the binary's own. */
export const newerSchemaVersion = builtSchemaVersion + 1;

/** The schema one process serves now, read through `GET /api/v1/schema/blocks`, which answers the
 *  installed value to any credential, here the harness's agent token. With `process` the read is
 *  pinned to that process of the two-process lane. */
export async function installedSchema(process?: DispatchProcess): Promise<BlockSchema> {
  const response = await fetch(new URL("/api/v1/schema/blocks", baseUrl), {
    headers: {
      Authorization: `Bearer ${e2eAgentToken}`,
      ...(process === undefined ? {} : pinnedTo(process)),
    },
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

/** Whether the schema one process serves declares the block type `name`. */
export async function servesType(name: string, process?: DispatchProcess): Promise<boolean> {
  return (await installedSchema(process)).types.some((type) => type.name === name);
}

/** Whether the schema one process serves declares the added type. */
export async function servesAddedType(process?: DispatchProcess): Promise<boolean> {
  return servesType(ADDED_TYPE, process);
}

/** Puts the store back at the schema it ships with - the binary's own version, with neither type
 *  the migration adds - in one transaction: writing the nodes is what announces the change, and
 *  two writes outside a transaction announce it twice. The version is written only where it moves,
 *  so a restore that changes nothing announces nothing and no process reinstalls a schema it
 *  already serves. A caller runs it and then waits for the processes to install what it leaves. */
export async function restoreStoreSchema(): Promise<void> {
  await sql(
    "begin",
    `delete from graph_tuples where from_kind = 'attribute' and from_id like '${ADDED_TYPE}/%'`,
    `delete from graph_nodes where (kind = 'supertag' and id in ('${ADDED_TYPE}', '${NEWER_TYPE}'))
       or (kind = 'attribute' and id like '${ADDED_TYPE}/%')`,
    `update graph_nodes set attributes = jsonb_set(attributes, '{version}', to_jsonb(${builtSchemaVersion}::int))
     where kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false)
       and attributes->>'version' is distinct from '${builtSchemaVersion}'`,
    "commit"
  );
}

/** The writer a migration's rows carry, as 20261109051232's seed carries one of its own. */
const migrationWriter = `'{"kind": "system", "id": "e2e-supertag-migration"}'::jsonb`;

/**
 * The supertag migration these specs apply: it adds the two types and raises every block type's
 * version to the newer one, in one transaction, so the commit is the one moment the store's schema
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
           'version', ${newerSchemaVersion}::int, 'position', 99),
         now(), ${migrationWriter}, 0),
       ('attribute', '${ADDED_TYPE}/supports', 'attribute', 'project', 'live',
         jsonb_build_object('name', 'supports', 'store_name', 'supports', 'position', 0,
           'kind', 'block[]', 'targets', jsonb_build_array('requirement')),
         now(), ${migrationWriter}, 0),
       ('supertag', '${NEWER_TYPE}', 'supertag', 'project', 'live',
         jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
           'version', ${newerSchemaVersion}::int, 'position', 98,
           'since', ${newerSchemaVersion}::int, 'creatable_since', ${newerSchemaVersion}::int),
         now(), ${migrationWriter}, 0)
     on conflict (kind, id) do update set state = 'live', attributes = excluded.attributes`,
    `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
     select 'attribute', '${ADDED_TYPE}/supports', 'attribute_of', 'supertag', '${ADDED_TYPE}',
            now(), ${migrationWriter}, 0, null, true
     where not exists (
       select 1 from graph_tuples
       where from_kind = 'attribute' and from_id = '${ADDED_TYPE}/supports' and kind = 'attribute_of'
     )`,
    `update graph_nodes set attributes = jsonb_set(attributes, '{version}', to_jsonb(${newerSchemaVersion}::int))
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
