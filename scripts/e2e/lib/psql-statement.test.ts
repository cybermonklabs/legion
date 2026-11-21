// psqlStatement hands psql the connection URI without its password and the password in PGPASSWORD
// alone, so no argument list of the run holds the database's password: a stand-in psql on PATH
// records the arguments and the PGPASSWORD it was started with.
import { expect, test } from "bun:test";
import { chmodSync, readFileSync, writeFileSync } from "node:fs";
import { delimiter, join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { psqlStatement } from "./psql-statement";

const scratch = temporaryDirectory("psql-statement");

test("psql is handed the connection without its password, and the password in PGPASSWORD", async () => {
  const recorded = join(scratch, "argv");
  const password = join(scratch, "pgpassword");
  const psql = join(scratch, "psql");
  writeFileSync(
    psql,
    `#!/bin/sh\nprintf '%s\\n' "$@" > '${recorded}'\nprintf '%s' "$PGPASSWORD" > '${password}'\necho 1\n`
  );
  chmodSync(psql, 0o755);
  const secret = "s3cr3t:p@ss/word";
  const uri = `postgresql://verifier:${encodeURIComponent(secret)}@127.0.0.1:5999/results?sslmode=disable`;
  const printed = await psqlStatement(uri, "select 1", {
    ...process.env,
    PATH: `${scratch}${delimiter}${process.env.PATH ?? ""}`,
  });
  expect(printed).toBe("1");
  const argv = readFileSync(recorded, "utf8");
  expect(argv).not.toContain(secret);
  expect(argv).not.toContain(encodeURIComponent(secret));
  expect(argv.split("\n")[0]).toBe("postgresql://verifier@127.0.0.1:5999/results?sslmode=disable");
  expect(readFileSync(password, "utf8")).toBe(secret);
});
