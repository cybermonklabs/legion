// scripts/e2e/lib/github-review-webhook.ts: a pull request review delivered to an Envoy listener's
// GitHub webhook route as GitHub delivers one, for a live proof whose daemon must hear a review
// the way a deployment's does (the answer loop's rig, scripts/e2e/answer-loop.sh, A19). A running
// daemon reads a pull request's listed reviews only as its workflow starts and after a handoff is
// recorded (packages/daemon/internal/daemon/issue_reads.go, readIssuePullRequest); every other
// review reaches it as GitHub's `pull_request_review` webhook, which the listener's
// `/webhook/github` route verifies and publishes for the daemon's GitHub intake.
//
//   bun scripts/e2e/lib/github-review-webhook.ts <secret file> <review.json> <pull.json> <url> [<event.json>]
//     prints {"delivery":"<uuid>","status":N,"answer":"<the route's body>"}, and exits 0 when the
//     route answered 2xx and 1 when it answered anything else
//
// <review.json> and <pull.json> are GitHub's REST answers for the review
// (GET /repos/<owner>/<repo>/pulls/<number>/reviews/<id>) and its pull request
// (GET /repos/<owner>/<repo>/pulls/<number>), which the caller reads itself. The event is the one
// GitHub sends when that review is submitted: action `submitted`, the review with its state
// lowercased as the webhook spells it, the pull request, its base repository as `repository`, and
// the review's author as `sender`. Its body is signed with HMAC-SHA256 under the secret the file
// holds, trimmed as the listener trims ENVOY_GITHUB_WEBHOOK_SECRET, and sent with
// `X-Hub-Signature-256: sha256=<hex>`, `X-GitHub-Event: pull_request_review` and a fresh
// `X-GitHub-Delivery`, the three headers GitHubHandler reads (packages/envoy/internal/webhook). The
// exact bytes signed are the bytes sent, and are written to <event.json> when it is given. The
// secret is printed nowhere. An event that lacks a field the listener or the daemon reads is
// refused before anything is sent (exit 2), naming the field.
import { createHmac, randomUUID } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";

const usage = `usage:
  bun scripts/e2e/lib/github-review-webhook.ts <secret file> <review.json> <pull.json> <url> [<event.json>]`;

type Json = Record<string, unknown>;

/** Every field of the event the listener reads into the envelope it publishes, which the daemon
 * decodes (packages/envoy/internal/contracts/normalize.go: githubRepo, githubNumber and
 * githubPayload's `pull_request_review` case; packages/daemon/internal/intake/decode.go,
 * decodeReview), and the sender GitHubHandler logs. */
export const readFields: readonly (readonly string[])[] = [
  ["action"],
  ["review", "id"],
  ["review", "user", "login"],
  ["review", "state"],
  ["review", "body"],
  ["review", "commit_id"],
  ["review", "submitted_at"],
  ["review", "html_url"],
  ["pull_request", "number"],
  ["pull_request", "title"],
  ["pull_request", "head", "sha"],
  ["repository", "full_name"],
  ["repository", "name"],
  ["repository", "owner", "login"],
  ["sender", "login"],
  ["sender", "type"],
];

/** The value at `keys` inside `value`, undefined where any step is not an object. */
export function at(value: unknown, keys: readonly string[]): unknown {
  let here = value;
  for (const key of keys) {
    if (here === null || typeof here !== "object") return undefined;
    here = (here as Json)[key];
  }
  return here;
}

function object(value: unknown, what: string): Json {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${what} is not a JSON object`);
  }
  return value as Json;
}

/** The `pull_request_review` event GitHub sends when `review`, a review of `pull`, is submitted,
 * from GitHub's REST answers for the two. It throws, naming the field, when the event would lack
 * one the listener or the daemon reads (readFields). */
export function reviewEvent(review: unknown, pull: unknown): Json {
  const rest = object(review, "the review");
  const pullRequest = object(pull, "the pull request");
  const state = rest.state;
  const event: Json = {
    action: "submitted",
    review: { ...rest, state: typeof state === "string" ? state.toLowerCase() : state },
    pull_request: pullRequest,
    repository: at(pullRequest, ["base", "repo"]),
    sender: rest.user,
  };
  for (const keys of readFields) {
    const value = at(event, keys);
    if (typeof value !== "string" && typeof value !== "number") {
      throw new Error(`the event has no ${keys.join(".")}`);
    }
  }
  return event;
}

/** GitHub's signature of `body` under `secret`: `sha256=` and the hex HMAC-SHA256, the value
 * GitHub sends as X-Hub-Signature-256 and the listener's verify.Github recomputes. */
export function signature(secret: string, body: string | Uint8Array): string {
  return `sha256=${createHmac("sha256", secret).update(body).digest("hex")}`;
}

/** Posts `body` to `url` as GitHub delivers a `pull_request_review` event, signed under `secret`,
 * and answers with the delivery id it named and the route's answer. */
export async function deliver(url: string, secret: string, body: string) {
  const delivery = randomUUID();
  const response = await fetch(url, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-github-event": "pull_request_review",
      "x-github-delivery": delivery,
      "x-hub-signature-256": signature(secret, body),
    },
    body,
    signal: AbortSignal.timeout(30_000),
  });
  return { delivery, status: response.status, answer: (await response.text()).trim() };
}

if (import.meta.main) {
  const [secretFile, reviewFile, pullFile, url, eventFile, ...extra] = process.argv.slice(2);
  if (!secretFile || !reviewFile || !pullFile || !url || extra.length > 0) {
    console.error(usage);
    process.exit(2);
  }
  let body: string;
  let secret: string;
  try {
    secret = readFileSync(secretFile, "utf8").trim();
    if (secret === "") throw new Error(`${secretFile} holds no secret`);
    body = JSON.stringify(
      reviewEvent(
        JSON.parse(readFileSync(reviewFile, "utf8")),
        JSON.parse(readFileSync(pullFile, "utf8"))
      )
    );
    if (eventFile) writeFileSync(eventFile, body);
  } catch (error) {
    console.error(
      `github-review-webhook: ${error instanceof Error ? error.message : String(error)}`
    );
    process.exit(2);
  }
  try {
    const answer = await deliver(url, secret, body);
    console.log(JSON.stringify(answer));
    if (answer.status < 200 || answer.status > 299) process.exit(1);
  } catch (error) {
    console.error(
      `github-review-webhook: ${error instanceof Error ? error.message : String(error)}`
    );
    process.exit(1);
  }
}
