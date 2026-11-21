import { randomUUID } from "node:crypto";
import { SQL } from "bun";

// A PostgreSQL database of a test's own. A Bun test that writes to PostgreSQL writes to a database
// of its own and never to the database a variable of the environment names: that one is the
// server's shared database, which another test binary, another file or another role of the pod
// writes too. The server is the one LEGION_TEST_PG_DSN names, the variable the pod and CI give
// every suite, or DISPATCH_TEST_DATABASE_URL's for a caller that names it, as the Dispatch stack
// of packages/envoy-client does because the job that runs it sets that one. A suite that takes its
// database from here reads neither variable itself; three files of packages/dispatch's
// e2e/__tests__ read LEGION_TEST_PG_DSN themselves, for the psql and the driver they test:
// harness-stacks.test.ts runs a `select 1` on the server's own database, and psql-session.test.ts
// and replace-blocks-guard.test.ts make and drop databases of their own.

/** The environment variables that may name the test server. */
export type ServerVariable = "LEGION_TEST_PG_DSN" | "DISPATCH_TEST_DATABASE_URL";

/** A database made for one caller on the test server. */
export interface OwnDatabase {
  /** Its name: the caller's prefix, an underscore and 32 random hex digits. */
  readonly name: string;
  /** The server's URL with this database as its path, which a client or a child process opens. */
  readonly url: string;
  /**
   * Drops the database with force, so a connection a client or a child left open does not keep
   * it on the server. A second call finds nothing to drop.
   */
  drop(): Promise<void>;
}

/** Whether `variable` names a test server, for a suite that runs only where one is. */
export function available(variable: ServerVariable = "LEGION_TEST_PG_DSN"): boolean {
  const server = process.env[variable];
  return server !== undefined && server !== "";
}

/**
 * Makes a database of the caller's own on the server `variable` names and returns its name, its
 * URL and its drop. The database is made `strategy file_copy`, a copy of template1's files, as the
 * Go tests' own fixture makes one: it writes no WAL for the copy, and every database made from the
 * pod's or CI's template1 has pgvector.
 *
 * It throws, naming the variable, where the variable is unset or empty or holds no URL, and
 * naming `prefix` where that is not a lowercase letter followed by at most 29 lowercase letters,
 * digits and underscores: the name goes into the statement unquoted and must fit PostgreSQL's 63
 * bytes. Neither refusal prints the server's URL, which carries its password.
 */
export async function ownDatabase(
  prefix: string,
  variable: ServerVariable = "LEGION_TEST_PG_DSN"
): Promise<OwnDatabase> {
  if (!/^[a-z][a-z0-9_]{0,29}$/.test(prefix)) {
    throw new TypeError(
      `ownDatabase's prefix ${JSON.stringify(prefix)} is not a lowercase letter followed by at ` +
        "most 29 lowercase letters, digits and underscores"
    );
  }
  const server = process.env[variable];
  if (server === undefined || server === "") {
    throw new Error(
      `${variable} must name the PostgreSQL server a test makes a database of its own on, and ` +
        "it is not set"
    );
  }
  let url: URL;
  try {
    url = new URL(server);
  } catch {
    throw new Error(
      `${variable} holds no URL: a test's own database is given as the server's URL with that ` +
        "database as its path"
    );
  }
  const name = `${prefix}_${randomUUID().replaceAll("-", "")}`;
  url.pathname = `/${name}`;
  await onServer(server, `create database ${name} strategy file_copy`);
  return {
    name,
    url: url.toString(),
    drop: () => onServer(server, `drop database if exists ${name} with (force)`),
  };
}

/** Runs one statement on the server's own database, over a connection of its own. */
async function onServer(server: string, statement: string): Promise<void> {
  const admin = new SQL(server);
  try {
    await admin.unsafe(statement);
  } finally {
    await admin.close();
  }
}
