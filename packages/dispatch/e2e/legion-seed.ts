// The Settings page's Legion daemon field, written straight into Postgres. No dashboard page draws
// a daemon's board, so the only Legion surface a browser drives is the Settings section where a
// project's daemon is named, listed and removed. These are the rows that section reads: a project
// to name a daemon for, and the `legion_settings` row a naming leaves.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import type { RowKey } from "./api";
import { sql } from "./psql";

const SEED_ACTOR = `'{"kind":"user","id":"alice"}'`;

/** The project, under the row's own key `project` (`rowKey("ACME")`, e2e/test.ts), which the
 *  Settings page's `Legion project` picker lists and the daemon routes write under, with alice as
 *  its default assignee: every emptied database holds her (`resetLogins`, e2e/api.ts, which the
 *  reset of e2e/seed.ts writes). No daemon is named: the spec about naming one starts from here.
 *  The insert has no `ON CONFLICT`, so a second project under one key fails the row rather than
 *  leaving the first in its place. */
export async function seedLegionProject(project: RowKey): Promise<void> {
  await sql(
    `INSERT INTO projects (key, name, default_assignee) VALUES ('${project}', 'Acme', 'alice')`
  );
}

/** `seedLegionProject` with the project's daemon already named at `stateUrl`, so the Settings
 *  list shows it and its Remove has a row to take away. */
export async function seedLegionDaemon(project: RowKey, stateUrl: string): Promise<void> {
  await seedLegionProject(project);
  await sql(
    `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
       VALUES ('${project}', '${stateUrl}', ${SEED_ACTOR}, now())`
  );
}

/** Takes `project`'s daemon out again, as its Remove does, for the `finally` of a row that named
 *  one: the next row of the worker reads the list this row started on. */
export async function removeLegionDaemon(project: string): Promise<void> {
  await sql(`DELETE FROM legion_settings WHERE project = '${project}'`);
}
