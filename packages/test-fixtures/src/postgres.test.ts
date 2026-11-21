import { describe, expect, test } from "bun:test";
import { SQL } from "bun";
import { captureEnvironment } from "./environment";
import { available, ownDatabase } from "./postgres";

// ownDatabase's refusals hold on any host. The database it makes needs a server, which
// LEGION_TEST_PG_DSN names in a pod and in every CI job that gives one; a host with none skips
// that block, saying so.

test("a prefix that is no lowercase name is refused before the server is asked", async () => {
  await expect(ownDatabase("Message-Wait")).rejects.toThrow(
    'ownDatabase\'s prefix "Message-Wait" is not a lowercase letter followed by at most 29 ' +
      "lowercase letters, digits and underscores"
  );
  await expect(ownDatabase("a".repeat(31))).rejects.toThrow("is not a lowercase letter");
});

test("an unset server variable is refused by its name", async () => {
  const restore = captureEnvironment(["DISPATCH_TEST_DATABASE_URL"]);
  try {
    process.env["DISPATCH_TEST_DATABASE_URL"] = "";
    expect(available("DISPATCH_TEST_DATABASE_URL")).toBe(false);
    await expect(ownDatabase("refused", "DISPATCH_TEST_DATABASE_URL")).rejects.toThrow(
      "DISPATCH_TEST_DATABASE_URL must name the PostgreSQL server a test makes a database of its " +
        "own on, and it is not set"
    );
  } finally {
    restore();
  }
});

test("a server variable that holds no URL is refused without printing it", async () => {
  const restore = captureEnvironment(["DISPATCH_TEST_DATABASE_URL"]);
  try {
    process.env["DISPATCH_TEST_DATABASE_URL"] = "host=localhost password=hunter2";
    const refusal = ownDatabase("refused", "DISPATCH_TEST_DATABASE_URL");
    await expect(refusal).rejects.toThrow("DISPATCH_TEST_DATABASE_URL holds no URL");
    await expect(refusal).rejects.not.toThrow("hunter2");
  } finally {
    restore();
  }
});

const withoutServer = !available();
if (withoutServer) {
  console.warn(
    `Skipped the database block of ${import.meta.path}: LEGION_TEST_PG_DSN names no server here.`
  );
}

describe.skipIf(withoutServer)("ownDatabase on the server LEGION_TEST_PG_DSN names", () => {
  test("makes a database of its own, which its URL opens and its drop removes", async () => {
    const database = await ownDatabase("test_fixtures_postgres");
    expect(database.name).toMatch(/^test_fixtures_postgres_[0-9a-f]{32}$/);
    const client = new SQL(database.url);
    try {
      const [row] = await client`select current_database() as name`;
      expect(row.name).toBe(database.name);
    } finally {
      await client.close();
    }
    // The drop is the one that ends a client the test left open: it drops with force.
    const left = new SQL(database.url);
    await left`select 1`;
    await database.drop();
    await left.close();
    const after = new SQL(database.url);
    try {
      await expect(after`select 1`).rejects.toThrow(`database "${database.name}" does not exist`);
    } finally {
      await after.close();
    }
    await database.drop();
  });
});
