import { createHash } from "node:crypto";
import type { CaseRow } from "./build-corpus";
import type { DispatchEvent } from "./dispatch-read";

/**
 * The reserve of INFRA-2897's next final test: the cases no command of this directory reads
 * before that round's claim run.
 *
 * The rule was registered on 2026-11-14, before its cut, under dispatch://INFRA-3189's approved
 * method ("Live and final test apart", "Grouping"):
 * dispatch://INFRA/artifact/infra-2897-final-test-reserve-md. An issue is reserved when it was
 * created at or after `RESERVE_CUT` and the first hexadecimal digit of the sha256 of
 * `RESERVE_TEXT` followed by its key is 0 to 7. The issue is the unit, as the grouping rule makes
 * it and as the rule's fifth item reads it: every case on a reserved issue is held, its approval
 * requests and its questions alike, so that no development read learns what the person answered
 * on an issue the final test will draw. An issue that a read has already spent is in
 * `MOVED_TO_DEVELOPMENT`, with the record of that read, and is held no more.
 *
 * `readCorpus` (`corpus-io.ts`) holds the reserved cases out of every corpus it reads, so every
 * command here skips them, unless `APPROVAL_EVAL_RESERVE` is `read:<reference>`. The reference
 * names the claim run that draws the final test, or the written release that ends the reserve.
 */

/** The cut: an issue created at or after it can be reserved. */
export const RESERVE_CUT = "2026-11-14T06:00:00Z";

/** The text the issue key is appended to before it is hashed, as the registration writes it. */
export const RESERVE_TEXT = "superfan-experiments INFRA-2897 reserve 2026-11-14:";

/** The environment variable that lets a command read the reserved cases. */
export const RESERVE_VARIABLE = "APPROVAL_EVAL_RESERVE";

/**
 * The reserved issues a read has already spent, each with the record that says so. The rule's
 * fifth item moves such an issue into the development data, whole.
 */
export const MOVED_TO_DEVELOPMENT: Readonly<Record<string, string>> = {
  "INFRA-3689":
    "a question's answer read on 2026-11-15 at 04:24Z by the super fan of the live experiments " +
    "(dispatch://INFRA/artifact/superfan-experiments-log-md, round 3, c4)",
};

const CUT_TIME = Date.parse(RESERVE_CUT);

/** The hexadecimal digits that pick a key: half of the sixteen. */
const PICKING_DIGITS = "01234567";

/** Whether the rule's hash picks an issue key, whatever the issue's creation time. */
export function pickedKey(issueKey: string): boolean {
  const digest = createHash("sha256").update(`${RESERVE_TEXT}${issueKey}`, "utf8").digest("hex");
  return PICKING_DIGITS.includes(digest.charAt(0));
}

/**
 * Whether a case is held by the reserve.
 *
 * `held` and `kept` are the rule's answer. `unknown` is a case the rule could hold that carries
 * no `issueCreatedAt` at all: its corpus was built before `build-corpus.ts` recorded when each
 * issue was created, and only a new build can say. A case answered before the cut is never held,
 * since an issue created at or after the cut has no answer before it. An issue whose creation the
 * build did not find (`issueCreatedAt` null), or whose time does not parse, is held: nothing shows
 * that it was created before the cut.
 */
export function reserveOf(row: CaseRow): "held" | "kept" | "unknown" {
  const key = row.issueKey;
  if (key === null || !pickedKey(key) || Object.hasOwn(MOVED_TO_DEVELOPMENT, key)) return "kept";
  const answered = Date.parse(row.answeredAt);
  if (!Number.isNaN(answered) && answered < CUT_TIME) return "kept";
  if (row.issueCreatedAt === undefined) return "unknown";
  if (row.issueCreatedAt === null) return "held";
  const created = Date.parse(row.issueCreatedAt);
  return Number.isNaN(created) || created >= CUT_TIME ? "held" : "kept";
}

/**
 * What `APPROVAL_EVAL_RESERVE` asks: `null` when it is unset or empty, so the reserve is held;
 * the reference when it is `read:<reference>`; and `undefined` for any other value, which a
 * caller refuses rather than guess at.
 */
export function readReferenceOf(value: string | undefined): string | null | undefined {
  const trimmed = value?.trim() ?? "";
  if (trimmed === "") return null;
  if (!trimmed.startsWith("read:")) return undefined;
  const reference = trimmed.slice("read:".length).trim();
  return reference === "" ? undefined : reference;
}

/**
 * Records an issue's creation time from an `issue.created` event of the log, keeping the first
 * one a key has. The payload's own `created_at` is the issue's; the event's is the fallback.
 */
export function noteIssueCreated(times: Map<string, string>, event: DispatchEvent): void {
  if (event.type !== "issue.created" || event.issue_key === null) return;
  if (times.has(event.issue_key)) return;
  const own = event.payload.created_at;
  times.set(event.issue_key, typeof own === "string" ? own : event.created_at);
}
