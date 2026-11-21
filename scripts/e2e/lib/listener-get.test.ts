import { afterEach, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// lib/listener-get.ts, the read a stage proof makes of a listener that takes no shared token
// (stage4b-sandbox-tree.sh's interests sampler and its controller-role lookup): one GET, presented
// with this machine's own broker token under DPoP, as every session's client presents it
// (packages/envoy-client's brokerCredential over packages/cipher-client's token client). Each row
// runs the helper as the proof runs it, a process of its own, against a stub broker that answers
// every trade with a token of its own minting and a stub listener that keeps every request it is
// sent, and judges what the listener received.
const helper = join(import.meta.dir, "listener-get.ts");
const dirs = temporaryDirectories("listener-get-test");
const servers: { stop: (force?: boolean) => void }[] = [];
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

/** A broker that answers every trade with a fresh DPoP access token, and the tokens it minted. */
function stubBroker(): { url: string; minted: string[] } {
  const minted: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      const token = `broker-access-${crypto.randomUUID()}`;
      minted.push(token);
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
        credential_id: "cred-1",
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, minted };
}

/** One request the stub listener was sent: its path, Authorization, and its DPoP proof's claims. */
interface Received {
  path: string;
  authorization: string;
  proof: Record<string, unknown>;
}

/** A listener that answers one session's interests 200 and every other path 404. */
function stubListener(): { url: string; received: Received[] } {
  const received: Received[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const path = new URL(request.url).pathname;
      const parts = (request.headers.get("DPoP") ?? "").split(".");
      received.push({
        path,
        authorization: request.headers.get("Authorization") ?? "",
        proof:
          parts.length === 3
            ? JSON.parse(Buffer.from(parts[1] ?? "", "base64url").toString("utf8"))
            : {},
      });
      if (path === "/v1/interests/s-1") return Response.json({ session_id: "s-1", topics: ["a"] });
      return Response.json({ error: "not found" }, { status: 404 });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, received };
}

/** A machine with an identity to trade at broker and no person signed in on it. */
function machine(broker: string): Record<string, string> {
  const home = dirs();
  const token = join(home, "projected-token");
  writeFileSync(token, `projected-${crypto.randomUUID()}\n`);
  return {
    PATH: process.env.PATH ?? "",
    HOME: home,
    AGENT_SECRETS_URL: broker,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: token,
    AGENT_SECRETS_SESSION: "listener-get-test",
  };
}

/** Runs the helper with env, its body to a file of its own, and what it printed and exited with.
 *  It runs as a process of its own while this process's stubs answer it, so it is not waited for
 *  synchronously. */
async function get(env: Record<string, string>, url: string) {
  const out = join(dirs(), "body.json");
  const child = Bun.spawn(["bun", helper, "--out", out, "--max-time", "20", url], {
    env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stdout, stderr, out };
}

test("a read presents the token the broker minted under DPoP, its proof naming GET and the URL", async () => {
  const broker = stubBroker();
  const listener = stubListener();
  const url = `${listener.url}/v1/interests/s-1`;
  const { code, stdout, stderr, out } = await get(machine(broker.url), url);
  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toBe("200\n");
  expect(JSON.parse(readFileSync(out, "utf8"))).toEqual({ session_id: "s-1", topics: ["a"] });
  expect(broker.minted).toHaveLength(1);
  expect(listener.received).toHaveLength(1);
  const [request] = listener.received;
  expect(request?.authorization).toBe(`DPoP ${broker.minted[0]}`);
  expect(request?.proof.htm).toBe("GET");
  expect(request?.proof.htu).toBe(url);
  expect(typeof request?.proof.ath).toBe("string");
}, 60_000);

test("a 404 is an answer: its status printed, its body kept, exit 0", async () => {
  const listener = stubListener();
  const { code, stdout, out } = await get(
    machine(stubBroker().url),
    `${listener.url}/v1/roles/absent`
  );
  expect(code).toBe(0);
  expect(stdout).toBe("404\n");
  expect(JSON.parse(readFileSync(out, "utf8"))).toEqual({ error: "not found" });
}, 60_000);

test("a machine with no broker route refuses, naming the gap, and sends the listener nothing", async () => {
  const listener = stubListener();
  const env = machine(stubBroker().url);
  delete env.AGENT_SECRETS_URL;
  const { code, stdout, stderr } = await get(env, `${listener.url}/v1/sessions`);
  expect(code).toBe(2);
  expect(stdout).toBe("");
  expect(stderr).toContain("AGENT_SECRETS_URL names no secrets broker");
  expect(listener.received).toHaveLength(0);
}, 60_000);

test("a listener that does not answer is exit 1, naming the path and never the host", async () => {
  const gone = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response("") });
  const url = `http://127.0.0.1:${gone.port}/v1/sessions`;
  gone.stop(true);
  const { code, stdout, stderr } = await get(machine(stubBroker().url), url);
  expect(code).toBe(1);
  expect(stdout).toBe("");
  expect(stderr).toContain("GET /v1/sessions got no answer");
  expect(stderr).not.toContain("127.0.0.1");
}, 60_000);
