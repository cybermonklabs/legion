import { afterEach, describe, expect, test } from "bun:test";
import { createHmac, timingSafeEqual } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { at, readFields, reviewEvent, signature } from "./github-review-webhook";

// The delivery A19 of the answer loop's rig makes (scripts/e2e/answer-loop.sh): a review read from
// GitHub, built into the `pull_request_review` event GitHub sends, signed as GitHub signs one and
// posted to the listener's `/webhook/github` route. The signature is checked against GitHub's own
// documented test vector and against a reproduction of the listener's check
// (packages/envoy/internal/verify/github.go, Github); the body against every field the listener's
// normalizer and the daemon's decoder read.
const helper = join(import.meta.dir, "github-review-webhook.ts");
const data = join(import.meta.dir, "testdata", "answer-loop");
const reviewFile = join(data, "github-review.json");
const pullFile = join(data, "github-pull.json");
const review = JSON.parse(readFileSync(reviewFile, "utf8"));
const pull = JSON.parse(readFileSync(pullFile, "utf8"));
const dir = temporaryDirectory("github-review-webhook-test");
const secret = "rig-webhook-secret-0123456789abcdef";

/** The listener's check (verify.Github): `sha256=` and the hex HMAC-SHA256 of the body under the
 * secret, compared in constant time with the header as it arrived. */
function listenerVerifies(key: string, body: Uint8Array, header: string | null): boolean {
  if (header === null || !header.startsWith("sha256=")) return false;
  const expected = Buffer.from(`sha256=${createHmac("sha256", key).update(body).digest("hex")}`);
  const got = Buffer.from(header);
  return expected.length === got.length && timingSafeEqual(expected, got);
}

type Seen = { method: string; path: string; headers: Headers; body: Uint8Array };
const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

/** A stand-in of the listener's GitHub route: it keeps what reached it and answers as
 * GitHubHandler does, `401 invalid signature` for a body the secret did not sign and `200 ok`
 * for one it did. */
function route(seen: Seen[]) {
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const body = new Uint8Array(await request.arrayBuffer());
      seen.push({
        method: request.method,
        path: new URL(request.url).pathname,
        headers: request.headers,
        body,
      });
      if (!listenerVerifies(secret, body, request.headers.get("x-hub-signature-256"))) {
        return new Response("invalid signature\n", { status: 401 });
      }
      return new Response("ok");
    },
  });
  cleanups.push(() => server.stop(true));
  return `http://127.0.0.1:${server.port}/webhook/github`;
}

/** Runs the helper's command line to the end; it is spawned, never run synchronously, since the
 * stand-in route answers from this process's own event loop. */
async function cli(...args: string[]) {
  const child = Bun.spawn(["bun", helper, ...args], { stdout: "pipe", stderr: "pipe" });
  const [out, err, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, out, err };
}

function secretFile(name: string, value: string): string {
  const file = join(dir, name);
  writeFileSync(file, value, { mode: 0o600 });
  return file;
}

describe("the signature", () => {
  test("is GitHub's own for its documented test vector", () => {
    // docs.github.com, "Validating webhook deliveries": this secret and payload sign to this value.
    expect(signature("It's a Secret to Everybody", "Hello, World!")).toBe(
      "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
    );
  });

  test("verifies with the listener's check, and a wrong secret or body does not", () => {
    const body = JSON.stringify(reviewEvent(review, pull));
    const bytes = new TextEncoder().encode(body);
    expect(listenerVerifies(secret, bytes, signature(secret, body))).toBe(true);
    expect(listenerVerifies("another secret", bytes, signature(secret, body))).toBe(false);
    expect(listenerVerifies(secret, bytes, signature(secret, `${body} `))).toBe(false);
    expect(listenerVerifies(secret, bytes, signature(secret, body).slice("sha256=".length))).toBe(
      false
    );
  });
});

describe("the event", () => {
  test("carries every field the listener and the daemon read", () => {
    const event = reviewEvent(review, pull);
    for (const keys of readFields) {
      expect(["string", "number"]).toContain(typeof at(event, keys));
    }
    expect(event.action).toBe("submitted");
    // GitHub's REST answer spells the state in capitals and its webhook in lower case.
    expect(at(event, ["review", "state"])).toBe("approved");
    expect(at(event, ["review", "id"])).toBe(2147483648124);
    expect(at(event, ["review", "user", "login"])).toBe("example-review[bot]");
    expect(at(event, ["review", "commit_id"])).toBe("0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c");
    expect(at(event, ["review", "submitted_at"])).toBe("2026-11-08T22:40:00Z");
    expect(String(at(event, ["review", "body"]))).toContain('"session":"session-reviewer"');
    expect(at(event, ["pull_request", "number"])).toBe(41);
    expect(at(event, ["pull_request", "head", "sha"])).toBe(
      "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
    );
    expect(at(event, ["repository", "full_name"])).toBe("sjawhar/legion-smoke");
    expect(at(event, ["repository", "owner", "login"])).toBe("sjawhar");
    expect(event.sender).toEqual(review.user);
  });

  test("is refused, naming the field, when GitHub's answer lacks one the daemon reads", () => {
    const { submitted_at: _, ...unsubmitted } = review;
    expect(() => reviewEvent(unsubmitted, pull)).toThrow("the event has no review.submitted_at");
    const { base: __, ...baseless } = pull;
    expect(() => reviewEvent(review, baseless)).toThrow("the event has no repository.full_name");
    expect(() => reviewEvent({ message: "Not Found" }, pull)).toThrow("the event has no review.id");
    expect(() => reviewEvent([], pull)).toThrow("the review is not a JSON object");
  });
});

describe("the delivery", () => {
  test("posts the exact signed body with GitHub's headers, and prints the route's answer", async () => {
    const seen: Seen[] = [];
    const url = route(seen);
    const saved = join(dir, "delivered.event.json");
    const result = await cli(secretFile("secret", `${secret}\n`), reviewFile, pullFile, url, saved);
    expect(result.code).toBe(0);
    const answer = JSON.parse(result.out);
    expect(answer.status).toBe(200);
    expect(answer.answer).toBe("ok");
    expect(answer.delivery).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
    );
    expect(seen).toHaveLength(1);
    const [hit] = seen as [Seen];
    expect(hit.method).toBe("POST");
    expect(hit.path).toBe("/webhook/github");
    expect(hit.headers.get("x-github-event")).toBe("pull_request_review");
    expect(hit.headers.get("x-github-delivery")).toBe(answer.delivery);
    expect(hit.headers.get("content-type")).toBe("application/json");
    // The file's trailing newline is trimmed, as the listener trims its own secret.
    expect(listenerVerifies(secret, hit.body, hit.headers.get("x-hub-signature-256"))).toBe(true);
    expect(readFileSync(saved)).toEqual(Buffer.from(hit.body));
    expect(JSON.parse(new TextDecoder().decode(hit.body))).toEqual(reviewEvent(review, pull));
    expect(result.out + result.err).not.toContain(secret);
  });

  test("prints a refusal the route answered and exits 1", async () => {
    const seen: Seen[] = [];
    const url = route(seen);
    const result = await cli(
      secretFile("wrong", "not-the-route-secret"),
      reviewFile,
      pullFile,
      url
    );
    expect(result.code).toBe(1);
    const answer = JSON.parse(result.out);
    expect(answer.status).toBe(401);
    expect(answer.answer).toBe("invalid signature");
    expect(seen).toHaveLength(1);
    expect(result.out + result.err).not.toContain("not-the-route-secret");
  });

  test("sends nothing for an event that lacks a field, or with no secret", async () => {
    const seen: Seen[] = [];
    const url = route(seen);
    const notFound = join(dir, "not-found.json");
    writeFileSync(notFound, JSON.stringify({ message: "Not Found", status: "404" }));
    const saved = join(dir, "never.event.json");
    const missing = await cli(secretFile("secret", secret), notFound, pullFile, url, saved);
    expect(missing.code).toBe(2);
    expect(missing.err).toContain("the event has no review.id");
    expect(existsSync(saved)).toBe(false);
    const empty = await cli(secretFile("empty", "\n"), reviewFile, pullFile, url);
    expect(empty.code).toBe(2);
    expect(empty.err).toContain("holds no secret");
    expect(seen).toHaveLength(0);
    expect((await cli(reviewFile, pullFile)).code).toBe(2);
  });
});
