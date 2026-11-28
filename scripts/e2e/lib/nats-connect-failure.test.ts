import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { nkeys } from "nats";

// The operator's NATS helpers given a server URL that holds a credential, and a connect that
// fails: each names the failure and exits 1, and the credential reaches its output neither in the
// server it names (a log names a server by the setting or argument that holds it, never by its
// text) nor in nats.js's own error text. No server runs: port 99999 does not parse, nothing
// listens on 127.0.0.1:1, and the other hosts do not resolve.
//
// nats.js's text carries a credential in two of these forms, which are the rows that fail when
// the helper prints that text unredacted: an nkey seed pasted as the host, which its resolver
// error names lowercased, and a token holding an unencoded `/`, whose first segment the URL makes
// the host. Each URL's user info is joined here, so the source holds no credential-bearing URL
// for the push leak check to find.
const userinfo = (...parts: string[]) => `${parts.join(":")}@`;
const NAMED = "the server <nats-url> names";
const seed = new TextDecoder().decode(nkeys.createUser().getSeed());
const cases = [
  [
    "a URL that does not parse",
    `nats://${userinfo("user", "canary-pw-7f3a")}127.0.0.1:99999`,
    NAMED,
    "canary-pw-7f3a",
  ],
  [
    "a server that refuses a user and password",
    `nats://${userinfo("user", "canary-pw-7f3a")}127.0.0.1:1`,
    NAMED,
    "canary-pw-7f3a",
  ],
  [
    "a server that refuses a token",
    `nats://${userinfo("canary-token-7f3a")}127.0.0.1:1`,
    NAMED,
    "canary-token-7f3a",
  ],
  ["an nkey seed pasted as the server", `nats://${seed}:4222`, NAMED, seed],
  [
    "a token holding an unencoded /",
    `nats://${userinfo("canarytok/def+gh==")}127.0.0.1:4222`,
    NAMED,
    "canarytok",
  ],
] as const;

function run(script: string, ...args: string[]) {
  const result = Bun.spawnSync(["bun", join(import.meta.dir, script), ...args], {
    stdout: "pipe",
    stderr: "pipe",
    timeout: 30_000,
  });
  return {
    exitCode: result.exitCode,
    output: `${result.stdout.toString()}${result.stderr.toString()}`,
  };
}

describe.each(cases)("a failed NATS connect at %s", (_, url, named, secret) => {
  test("nats-stream names it and never prints the credential", () => {
    const result = run("nats-stream.ts", "last", url, "STREAM", "subject", "1");
    expect(result.exitCode).toBe(1);
    expect(result.output).toContain(`nats-stream: could not connect to NATS at ${named}: `);
    expect(result.output.toLowerCase()).not.toContain(secret.toLowerCase());
  });

  for (const [command, ...rest] of [
    ["publish", "subject", "data"],
    ["envelope", "session", "message-id", "1"],
  ]) {
    test(`dispatch-user-turns ${command} names it and never prints the credential`, () => {
      const [subjectOrSession, ...more] = rest;
      const result = run("dispatch-user-turns.ts", command, url, subjectOrSession, ...more);
      expect(result.exitCode).toBe(1);
      expect(result.output).toContain(
        `dispatch-user-turns: could not connect to NATS at ${named}: `
      );
      expect(result.output.toLowerCase()).not.toContain(secret.toLowerCase());
    });
  }
});
