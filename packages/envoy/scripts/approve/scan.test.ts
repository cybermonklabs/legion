// packages/envoy/scripts/approve/scan.test.ts
//
// The scan of scripts/approve/scan.ts (approve-check.sh step 1e) compares the thumbprint of
// "each private key, as PEM or JWK" it finds on the box with the bindings the broker logged. A
// signed-in box keeps its own key as a PEM inside a JSON string (the sign-in record's box_key), so
// a broker key that leaked in that same form must be found too. Each case runs the scan as
// approve-check runs it, as a process, against a broker log naming one binding and a box holding
// that key in one form; Dispatch is a stand-in that answers every diagnostic 401.
import { afterAll, beforeAll, expect, test } from "bun:test";
import { createHash, generateKeyPairSync, type KeyObject } from "node:crypto";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const SCAN = join(import.meta.dir, "scan.ts");

/** The stand-in Dispatch: the port it took, and its stop. */
let dispatch: { readonly port: number | undefined; stop(force?: boolean): void };
let work: string;

beforeAll(() => {
  dispatch = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => Response.json({ code: "BROKER_TOKEN_NOT_ACTIVE" }, { status: 401 }),
  });
  work = mkdtempSync(join(tmpdir(), "scan-"));
});

afterAll(() => {
  dispatch.stop(true);
  rmSync(work, { recursive: true, force: true });
});

function thumbprint(key: KeyObject): string {
  const jwk = key.export({ format: "jwk" }) as Record<string, string>;
  const members = { crv: jwk["crv"], kty: jwk["kty"], x: jwk["x"], y: jwk["y"] };
  return createHash("sha256").update(JSON.stringify(members)).digest("base64url");
}

async function scan(name: string, plant: (box: string, pem: string) => void) {
  const dir = join(work, name);
  const box = join(dir, "box");
  mkdirSync(box, { recursive: true });
  const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  const jkt = thumbprint(publicKey);
  const log = join(dir, "broker.log");
  writeFileSync(
    log,
    `${JSON.stringify({ message: "broker: minted a grant's token", grant: "g", token_sha256: "0".repeat(64), jkt })}\n`
  );
  plant(box, privateKey.export({ format: "pem", type: "sec1" }).toString());
  // Spawned, not spawnSync: the stand-in Dispatch answers the diagnostic from this process.
  const run = Bun.spawn([process.execPath, SCAN, log, box], {
    env: {
      PATH: process.env["PATH"] ?? "",
      DISPATCH_URL: "https://dispatch.approve.test",
      APPROVE_CHECK_DISPATCH_LOCAL: `http://127.0.0.1:${dispatch.port}`,
      APPROVE_CHECK_STATE: join(dir, "state.json"),
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
    run.exited,
  ]);
  expect(exitCode, stderr).toBe(0);
  const answer = JSON.parse(stdout) as { key_matches: string[]; private_keys: number };
  return { answer, jkt };
}

test("control: a logged binding's key as a bare PEM file on the box is found", {
  timeout: 60_000,
}, async () => {
  const { answer, jkt } = await scan("bare-pem", (box, pem) =>
    writeFileSync(join(box, "key.pem"), pem)
  );
  expect(answer.key_matches).toContain(jkt);
});

test("a logged binding's key as a PEM inside a JSON string, the form a sign-in record keeps its box key in, is found", {
  timeout: 60_000,
}, async () => {
  const { answer, jkt } = await scan("pem-in-json", (box, pem) =>
    writeFileSync(
      join(box, "record.json"),
      JSON.stringify({ person: "ada@example.com", box_key: pem })
    )
  );
  expect(answer.private_keys).toBeGreaterThan(0);
  expect(answer.key_matches).toContain(jkt);
});
