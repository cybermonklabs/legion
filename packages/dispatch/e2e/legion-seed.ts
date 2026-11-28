// The Settings page's Legion daemon field, written straight into Postgres. No dashboard page draws
// a daemon's board, so the only Legion surface a browser drives is the Settings section where a
// project's daemon is named, listed and removed. These are the rows that section reads: a project
// to name a daemon for, and the `legion_settings` row a naming leaves.
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { sql } from "./psql";

const SEED_ACTOR = `'{"kind":"user","id":"alice"}'`;

/** The project the Settings section's daemon field names a daemon for. */
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
 *  list shows it and its Remove has a row to take away. */
export async function seedLegionDaemon(stateUrl: string): Promise<void> {
  await seedLegionProject();
  await sql(
    `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
       VALUES ('${LEGION_PROJECT}', '${stateUrl}', ${SEED_ACTOR}, now())`
  );
}
