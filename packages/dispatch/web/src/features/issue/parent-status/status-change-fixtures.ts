import type { Mock } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { answerRoute } from "../../../__tests__/send-answers";
import { ApiError, type WriteBounds } from "../../../api/client";
import type { StatusChangeAnswer, StatusChangeInput } from "../../../api/types";

/** One answer of `POST /api/v1/issues/{key}/status-change` as the real server wrote it: its status
 *  and its body. */
interface Recorded {
  readonly status: number;
  readonly body: Record<string, unknown>;
}

/** The route's answers that packages/envoy/internal/dispatch/api's
 *  `TestStatusChangeAnswersMatchTheirFixtures` writes from the real server, read where they are
 *  and never copied, so a change of the route's answers reaches these tests. */
const FIXTURES = join(
  import.meta.dir,
  "..",
  "..",
  "..",
  "..",
  "..",
  "..",
  "envoy",
  "internal",
  "dispatch",
  "api",
  "testdata",
  "status-change"
);

/** The names of the recorded answers, each the file the Go test writes without its `.json`:
 *  `changed` (ACME-2 to `in_progress` with `include_parent` and `unlink_children`), `dry-run`
 *  (ACME-4 to `done` with `include_children`, a dry run), `conflict` (ACME-4 to `done` with no
 *  option, a child in work), `browser-only` (the same with `include_children` from a token),
 *  `conflict-midway` (a call refused after its first step) and `stale` (a stale
 *  `expected_sequence`). */
type StatusChangeFixture =
  | "browser-only"
  | "changed"
  | "conflict"
  | "conflict-midway"
  | "dry-run"
  | "stale";

/** Each recorded answer's text, read from its file once. Each call parses it anew, so no test
 *  changes what another reads. */
const fixtureTexts = new Map<StatusChangeFixture, string>();

function recorded(name: StatusChangeFixture): Recorded {
  let text = fixtureTexts.get(name);
  if (text === undefined) {
    text = readFileSync(join(FIXTURES, `${name}.json`), "utf8");
    fixtureTexts.set(name, text);
  }
  return JSON.parse(text) as Recorded;
}

/** The 200 body of `name`, as the page's client resolves it. */
export function fixtureAnswer(name: "changed" | "dry-run"): StatusChangeAnswer {
  const { body, status } = recorded(name);
  if (status !== 200) throw new Error(`${name}.json holds a ${status}, not an answer`);
  return body as unknown as StatusChangeAnswer;
}

/** The refusal of `name`, as the page's client rejects with it. */
export function fixtureRefusal(
  name: "browser-only" | "conflict" | "conflict-midway" | "stale"
): ApiError {
  const { body, status } = recorded(name);
  return new ApiError(status, body as ConstructorParameters<typeof ApiError>[1]);
}

/** One request of the route as a test answers it: the issue key, the body and the bounds. */
export type StatusChangeAnswerer = (
  key: string,
  input: StatusChangeInput,
  bounds: WriteBounds
) => Promise<StatusChangeAnswer>;

/** Every request of the route a test makes - each dry run and each status change - answered by
 *  one mock. */
export const answerStatusChanges = (): Mock<StatusChangeAnswerer> =>
  answerRoute<StatusChangeAnswerer>("POST", "/api/v1/issues/*/status-change");
