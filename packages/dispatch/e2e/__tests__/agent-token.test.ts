import { afterAll, describe, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { rowLimit, runBounded } from "./bounded-subprocess";

// e2e/run-server.sh gives its server the agent bearer `e2e-token`, which every row and acceptance
// script sends, unless the caller that starts it names a file holding another
// (DISPATCH_E2E_AGENT_TOKEN_FILE), and e2e/api.ts sends the same bearer. The skill-scenario rig
// names one, since its agents read their bearer and the harness's name is no part of the world
// they are shown. Catches: a caller's token the server is not given, a default that moved for
// every other caller, an unreadable or empty file served as the default, the file's name left in
// the server's environment, and an api.ts that sends a bearer other than the one its server takes.
const runServer = fileURLToPath(new URL("../run-server.sh", import.meta.url));
const api = fileURLToPath(new URL("../api.ts", import.meta.url));
const dispatchDirectory = fileURLToPath(new URL("../..", import.meta.url));
const directory = mkdtempSync(join(tmpdir(), "agent-token-"));
afterAll(() => rmSync(directory, { force: true, recursive: true }));

/** One child of a row, bounded by the shared `short` bound, as every row of this directory that
 * waits for a child bounds it (row-limits.ts), so the row's limit is read from its waits. */
async function run(argv: string[], env: Record<string, string>, cwd = directory) {
  const { code, stderr, stdout } = await runBounded({
    argv,
    bound: "short",
    cwd,
    env: { PATH: process.env.PATH ?? "", HOME: directory, TMPDIR: tmpdir(), ...env },
    from: import.meta.url,
  });
  return { code, stderr, stdout };
}

/** A file under this suite's directory holding `text`. */
function file(name: string, text: string): string {
  const written = join(directory, name);
  writeFileSync(written, text);
  return written;
}

describe("the harness's agent bearer", () => {
  // The server run-server.sh starts, standing in: it prints the bearer it was given and whether the
  // caller's file name reached it, which the sweep of every DISPATCH_* variable drops.
  const server = file(
    "envoy-dispatch",
    [
      "#!/usr/bin/env bash",
      'echo "token=$DISPATCH_AGENT_TOKEN file=$(printenv DISPATCH_E2E_AGENT_TOKEN_FILE || echo swept)"',
      "",
    ].join("\n")
  );
  chmodSync(server, 0o755);
  const started = (env: Record<string, string>) =>
    run(["bash", runServer], {
      DATABASE_URL: "postgres://postgres@127.0.0.1:1/never-opened",
      DISPATCH_E2E_AGENT_SECRETS_URL: "",
      DISPATCH_E2E_PORT: "8777",
      DISPATCH_E2E_SERVER_BIN: server,
      FAKE_ENVOY_PORT: "9021",
      FAKE_GITHUB_PORT: "9022",
      ...env,
    });
  const token = "4f1c9a0be27d53c86a1e0f94b7d2c3a5e6f708192a3b4c5d";

  test(
    "run-server.sh gives its server e2e-token, or the token the caller's file holds",
    async () => {
      expect(await started({})).toEqual({
        code: 0,
        stderr: "",
        stdout: "token=e2e-token file=swept\n",
      });
      const given = file("given-token", `${token}\n`);
      expect(await started({ DISPATCH_E2E_AGENT_TOKEN_FILE: given })).toEqual({
        code: 0,
        stderr: "",
        stdout: `token=${token} file=swept\n`,
      });
      const absent = join(directory, "absent");
      expect(await started({ DISPATCH_E2E_AGENT_TOKEN_FILE: absent })).toEqual({
        code: 2,
        stderr: `run-server.sh: DISPATCH_E2E_AGENT_TOKEN_FILE=${absent} cannot be read\n`,
        stdout: "",
      });
      const empty = file("empty-token", "\n");
      expect(await started({ DISPATCH_E2E_AGENT_TOKEN_FILE: empty })).toEqual({
        code: 2,
        stderr: `run-server.sh: DISPATCH_E2E_AGENT_TOKEN_FILE=${empty} holds no token\n`,
        stdout: "",
      });
    },
    rowLimit("short", "short", "short", "short")
  );

  test(
    "e2e/api.ts sends the bearer the server it started takes, and a deployed server's its own",
    async () => {
      // A process of its own per environment: api.ts reads its bearer when the module is evaluated,
      // which a static import does once, in this process's environment.
      const sent = async (env: Record<string, string>) =>
        (
          await run(
            ["bun", "-e", `console.log((await import(${JSON.stringify(api)})).e2eAgentToken)`],
            env,
            dispatchDirectory
          )
        ).stdout;
      const given = file("api-token", `${token}\n`);
      expect(await sent({})).toBe("e2e-token\n");
      expect(await sent({ DISPATCH_E2E_AGENT_TOKEN_FILE: given })).toBe(`${token}\n`);
      expect(
        await sent({
          DISPATCH_E2E_AGENT_TOKEN_FILE: given,
          E2E_AGENT_TOKEN: "deployed-token",
          PLAYWRIGHT_BASE_URL: "http://127.0.0.1:1",
        })
      ).toBe("deployed-token\n");
    },
    rowLimit("short", "short", "short")
  );
});
