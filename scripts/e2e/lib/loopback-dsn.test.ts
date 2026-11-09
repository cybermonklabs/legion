import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";

// loopback-dsn.py's tests: the helper reads each DSN with the real libpq of this host, so every
// case asserts the answer libpq gave the helper rather than a pattern of the DSN's text. A host
// with no python3 or no libpq skips them, naming what it lacks.

const helper = join(import.meta.dir, "loopback-dsn.py");
const variable = "LOOPBACK_DSN_TEST";
const what = "this run creates and drops databases there";
/** A password no refusal may print, in a DSN whose host libpq reads from after the `@`. */
const password = "hunter2";

/** What the helper answered: its exit status, the host it printed, and its refusal line. */
function guard(dsn: string): { code: number; host: string; refusal: string } {
  const run = Bun.spawnSync(["python3", helper, variable, what], {
    env: { ...process.env, [variable]: dsn },
    stderr: "pipe",
    stdout: "pipe",
  });
  return {
    code: run.exitCode,
    host: run.stdout.toString().trim(),
    refusal: run.stderr.toString().trim(),
  };
}

const withoutPython = skipWithout("python3");
// The helper refuses every DSN where this host has no libpq, and says so in that refusal.
const withoutLibpq =
  !withoutPython && guard("postgres://127.0.0.1:5433/postgres").refusal.includes("no libpq");
if (withoutLibpq) {
  console.warn(
    `Skipped the tests in ${import.meta.path}. The helper reads each DSN with libpq, and ` +
      `ctypes.util.find_library("pq") finds none on this host; install libpq5.`
  );
}
const skip = withoutPython || withoutLibpq;

describe.skipIf(skip)("loopback-dsn.py takes a loopback DSN", () => {
  test("the DSN a run is given prints its host", () => {
    const answer = guard(`postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable`);
    expect(answer).toEqual({ code: 0, host: "127.0.0.1", refusal: "" });
  });

  test("localhost and ::1 are loopback too", () => {
    expect(guard("postgres://localhost:5433/db")).toEqual({
      code: 0,
      host: "localhost",
      refusal: "",
    });
    expect(guard("postgres://[::1]:5433/db")).toEqual({ code: 0, host: "::1", refusal: "" });
  });
});

describe.skipIf(skip)("loopback-dsn.py refuses the host libpq reads", () => {
  // libpq ends the user name at the first `@`, and a `?` does not end it, so the host of each of
  // these two is evil.example while the authority up to the first `?` reads localhost.
  test("a `?` before the `@` does not end the user name", () => {
    const answer = guard(`postgres://localhost?sslmode=disable:${password}@evil.example/postgres`);
    expect(answer.code).toBe(1);
    expect(answer.host).toBe("");
    expect(answer.refusal).toBe(
      `${variable} must name 127.0.0.1, localhost or ::1: libpq reads the host evil.example,` +
        ` and ${what}`
    );
  });

  test("a bare `?@` does not end it either", () => {
    const answer = guard("postgres://localhost?@evil.example:5432/db");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toContain("libpq reads the host evil.example");
  });

  test("a percent-encoded host key reaches libpq as a host", () => {
    const answer = guard("postgres://localhost:5433/db?%68ost=evil.example");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toContain("libpq reads the host evil.example");
  });

  test("a remote host is refused by its own name", () => {
    const answer = guard("postgres://legion_test@db.prod.example:5432/postgres");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toContain("libpq reads the host db.prod.example");
  });

  test("a DSN with no host at all is refused", () => {
    const answer = guard("postgres:///postgres");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} names no host: libpq would read PGHOST or its own default, and ${what}`
    );
  });
});

describe.skipIf(skip)("loopback-dsn.py refuses what it cannot read as one host", () => {
  test("a newline hides the `host=` key from a line reader, not from libpq", () => {
    const answer = guard("postgres://localhost:5433/db?application_name=x\n&host=evil.example");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} holds the control character 0x0a: libpq reads the whole DSN, and a reader` +
        " that stops at the end of a line reads less of it"
    );
  });

  test("two hosts are two servers a drop can reach", () => {
    const answer = guard("postgres://localhost,evil.example:5432/db");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must name one host: libpq reads 2 (localhost, evil.example), and it connects` +
        " to the next one when the first does not answer"
    );
  });

  test("hostaddr names the server the host name does not", () => {
    const answer = guard("postgres://localhost:5433/db?hostaddr=10.0.0.1");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must carry no hostaddr parameter: libpq reads hostaddr=10.0.0.1 and connects` +
        " to that address, whatever the host name is"
    );
  });

  test("service reads a host out of ~/.pg_service.conf", () => {
    const answer = guard("postgres://localhost:5433/db?service=prod");
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(
      `${variable} must carry no service parameter: libpq reads service=prod from` +
        " ~/.pg_service.conf, and that entry can name any host"
    );
  });

  test("a DSN libpq cannot parse is refused, and its text is not printed", () => {
    const answer = guard(`postgres://legion_test:${password}@localhost:5433/db?=x`);
    expect(answer.code).toBe(1);
    expect(answer.refusal).toBe(`${variable} is no connection string libpq can parse`);
  });

  test("an unset variable is refused by name", () => {
    const run = Bun.spawnSync(["python3", helper, variable, what], {
      env: { ...process.env, [variable]: undefined },
      stderr: "pipe",
      stdout: "pipe",
    });
    expect(run.exitCode).toBe(1);
    expect(run.stderr.toString().trim()).toBe(`${variable} is not set`);
  });
});

describe.skipIf(skip)("loopback-dsn.py prints no userinfo", () => {
  // The password is in the userinfo of each refused DSN, and the user name of the first two is
  // the text a shell pattern reads as the host.
  const refused = [
    `postgres://localhost?sslmode=disable:${password}@evil.example/postgres`,
    `postgres://localhost?${password}@evil.example:5432/db`,
    `postgres://legion_test:${password}@evil.example:5432/db`,
    `postgres://legion_test:${password}@localhost:5433/db?%68ost=evil.example`,
    `postgres://legion_test:${password}@localhost,evil.example:5432/db`,
    `postgres://legion_test:${password}@localhost:5433/db?hostaddr=10.0.0.1`,
    `postgres://legion_test:${password}@localhost:5433/db?service=prod`,
    `postgres://legion_test:${password}@localhost:5433/db?application_name=x\n&host=evil.example`,
  ];
  for (const dsn of refused) {
    test(`no part of the userinfo of ${dsn.replace(password, "<password>")}`, () => {
      const answer = guard(dsn);
      expect(answer.code).toBe(1);
      expect(answer.refusal).not.toContain(password);
      expect(answer.refusal).not.toContain("legion_test");
      expect(answer.refusal).not.toContain("sslmode");
    });
  }
});
