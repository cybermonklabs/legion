import { describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { type InstanceBanner, instanceBanner, shellQuote } from "../local-instance-support";
import { rowLimit, runBounded } from "./bounded-subprocess";

const makeDirectory = temporaryDirectories("local-instance-banner");

// The banner names the database the instance truncates. A DATABASE_URL the person set may carry
// a user and a password: the banner names it with neither and with no query (`urlForText`),
// and its command reads DATABASE_URL from the shell that runs it, never the value, saying so in
// one line. So does each broker switch the shell set, of which the broker's URL may carry a user
// and a password too. The repository's dev container's URL, which the source holds with its
// password, is named as it is, in both. The password is made for this run.
describe("instanceBanner", () => {
  const banner = (
    databaseUrl: string,
    databaseGiven: boolean,
    env: Readonly<Record<string, string | undefined>> = {}
  ): InstanceBanner => ({
    baseUrl: "http://127.0.0.1:8777",
    broadcastId: "brd-1",
    plannerSession: "019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01",
    fakeEnvoy: "http://127.0.0.1:9020",
    databaseUrl,
    databaseGiven,
    ports: { DISPATCH_E2E_PORT: "8777" },
    env,
  });
  /** The banner's line that runs a spec, as a person pastes it. */
  const command = (text: string) =>
    text.split("\n").find((line) => line.startsWith("    DISPATCH_E2E_REUSE_SERVERS=1 ")) ?? "";

  test("a DATABASE_URL the person set is named with no user part, and the command reads it from the shell", () => {
    const password = crypto.randomUUID();
    const given = `postgres://made-up-user:${password}@127.0.0.1:5432/acme_local?sslmode=disable`;
    const text = instanceBanner(banner(given, true));
    expect(text).not.toContain(password);
    expect(text).not.toContain("made-up-user");
    expect(text).toContain(
      "  Database (truncated and reseeded on every start): postgres://127.0.0.1:5432/acme_local\n"
    );
    expect(command(text)).toBe(
      '    DISPATCH_E2E_REUSE_SERVERS=1 DATABASE_URL="$DATABASE_URL" DISPATCH_E2E_PORT=8777 bunx playwright test --config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts'
    );
    expect(text).toContain(
      "  The command reads DATABASE_URL as the shell that runs it sets it: set it there as you set it here.\n"
    );
  });

  // A URL in the dev container's form, as local-instance.ts builds it, with a placeholder in place
  // of its password: the banner names the URL that DATABASE_URL did not give as it is.
  test("the dev container's URL, which the source holds, is named as it is", () => {
    const container =
      "postgres://postgres:not-a-secret@127.0.0.1:55432/dispatch_local?sslmode=disable";
    const text = instanceBanner(banner(container, false));
    expect(text).toContain(`  Database (truncated and reseeded on every start): ${container}\n`);
    expect(command(text)).toContain(` DATABASE_URL=${shellQuote(container)} `);
    expect(text).not.toContain("The command reads DATABASE_URL");
  });

  test(
    "the command a person pastes runs with the DATABASE_URL their shell exported",
    async () => {
      const password = crypto.randomUUID();
      const given = `postgres://made-up-user:${password}@127.0.0.1:5432/acme_local?sslmode=disable`;
      const bin = makeDirectory();
      // A stand-in `bunx` that prints the DATABASE_URL it was started with.
      writeFileSync(join(bin, "bunx"), '#!/bin/sh\nprintf %s "$DATABASE_URL"\n', { mode: 0o755 });
      const { code, named, stderr, stdout } = await runBounded({
        argv: ["bash", "-c", command(instanceBanner(banner(given, true)))],
        bound: "short",
        env: { PATH: `${bin}:/usr/bin:/bin`, DATABASE_URL: given },
        from: import.meta.url,
      });
      if (code !== 0) throw new Error(`${named} exited ${code}: ${stderr}`);
      expect(stdout).toBe(given);
    },
    // The bash that runs the pasted command, with the stand-in under it.
    rowLimit("short")
  );

  test(
    "a broker URL with a user part is read from the shell, and the banner holds no part of it",
    async () => {
      const password = crypto.randomUUID();
      const given = `postgres://made-up-user:${password}@127.0.0.1:5432/acme_local`;
      const broker = `http://made-up-broker-user:${password}@127.0.0.1:13380`;
      const tokenFile = "/var/run/made-up/account-token";
      const text = instanceBanner(
        banner(given, true, {
          DISPATCH_E2E_AGENT_SECRETS_URL: broker,
          DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE: tokenFile,
        })
      );
      expect(text).not.toContain(password);
      expect(text).not.toContain("made-up-broker-user");
      expect(command(text)).toBe(
        // biome-ignore lint/suspicious/noTemplateCurlyInString: the pasted command's own shell expansions
        '    DISPATCH_E2E_REUSE_SERVERS=1 DATABASE_URL="$DATABASE_URL" DISPATCH_E2E_PORT=8777 env ${DISPATCH_E2E_AGENT_SECRETS_URL+"DISPATCH_E2E_AGENT_SECRETS_URL=$DISPATCH_E2E_AGENT_SECRETS_URL"} ${DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE+"DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"} bunx playwright test --config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts'
      );
      expect(text).toContain(
        "  The command reads DATABASE_URL, DISPATCH_E2E_AGENT_SECRETS_URL and DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE as the shell that runs it sets them: set them there as you set them here.\n"
      );
      const bin = makeDirectory();
      // A stand-in `bunx` that prints the broker switches it was started with.
      writeFileSync(
        join(bin, "bunx"),
        '#!/bin/sh\nprintf "%s %s" "$DISPATCH_E2E_AGENT_SECRETS_URL" "$DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"\n',
        { mode: 0o755 }
      );
      const { code, named, stderr, stdout } = await runBounded({
        argv: ["bash", "-c", command(text)],
        bound: "short",
        env: {
          PATH: `${bin}:/usr/bin:/bin`,
          DATABASE_URL: given,
          DISPATCH_E2E_AGENT_SECRETS_URL: broker,
          DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE: tokenFile,
        },
        from: import.meta.url,
      });
      if (code !== 0) throw new Error(`${named} exited ${code}: ${stderr}`);
      expect(stdout).toBe(`${broker} ${tokenFile}`);
    },
    // The bash that runs the pasted command, with the stand-in under it.
    rowLimit("short")
  );

  // The harness reads an empty broker switch as "no broker" and an unset one as "the fake broker"
  // (e2e/harness-broker.ts), so the pasted command, run in a shell that sets neither switch, leaves
  // each of them unset rather than empty. The control is the same command in a shell that sets
  // both, which hands both on (the row above).
  test(
    "a broker switch the shell that runs the command does not set is left unset",
    async () => {
      const text = instanceBanner(
        banner("postgres://made-up-user@127.0.0.1:5432/acme_local", true, {
          DISPATCH_E2E_AGENT_SECRETS_URL: "http://127.0.0.1:13380",
          DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE: "/var/run/made-up/account-token",
        })
      );
      const bin = makeDirectory();
      // A stand-in `bunx` that says, of each broker switch, whether it is set at all.
      writeFileSync(
        join(bin, "bunx"),
        // biome-ignore lint/suspicious/noTemplateCurlyInString: the stand-in's own shell expansions
        '#!/bin/sh\nprintf "[%s][%s]" "${DISPATCH_E2E_AGENT_SECRETS_URL+set}" "${DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE+set}"\n',
        { mode: 0o755 }
      );
      const { code, named, stderr, stdout } = await runBounded({
        argv: ["bash", "-c", command(text)],
        bound: "short",
        env: { PATH: `${bin}:/usr/bin:/bin`, DATABASE_URL: "postgres://127.0.0.1:5432/acme_local" },
        from: import.meta.url,
      });
      if (code !== 0) throw new Error(`${named} exited ${code}: ${stderr}`);
      expect(stdout).toBe("[][]");
    },
    // The bash that runs the pasted command, with the stand-in under it.
    rowLimit("short")
  );
});
