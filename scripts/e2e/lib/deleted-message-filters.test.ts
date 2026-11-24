import { describe, expect, test } from "bun:test";
import { fileURLToPath } from "node:url";
import { daemonActorEvent, noneNote } from "./multi-repo-acceptance-harness.ts";
import { runJq } from "./run-jq.ts";

// The jq filters the end-to-end proofs judge a posted message with, each run as its script runs it
// over an issue's events, in the order its script reads them: multi-repo-acceptance.sh's two
// newest first (its dispatch_events_raw, GET /api/v1/issues/<KEY>/events?order=desc), and the
// review-cap, stage3 and stage4b ones oldest first (lib/workflow.sh's dispatch_events, which pages
// through ?after=<seq>). Dispatch lets a message's author delete it (dispatch://INFRA-1523): an
// event read then gives its message.created with deleted_at set and no body. Each filter takes such
// a message as not posted, so a proof neither stops on its missing body nor counts it, and still
// finds a standing message that matches.

interface Event {
  seq: number;
  type: string;
  actor: { kind: string; id: string };
  payload: Record<string, unknown>;
}

/**
 * Runs lib/NAME.jq over EVENTS with ARGS as its `--arg` pairs, as its script does, and returns its
 * answer. A script that reads the exit status adds -e, which only turns a false answer into exit 1;
 * without it, a non-zero exit is a jq error, which runJq throws.
 */
function judge(name: string, args: Record<string, string>, events: readonly Event[]): unknown {
  const program = fileURLToPath(new URL(`./${name}.jq`, import.meta.url));
  const argv = Object.entries(args).flatMap(([key, value]) => ["--arg", key, value]);
  return JSON.parse(runJq([...argv, "-f", program], JSON.stringify(events)));
}

const project = "ACME";
const daemon = { kind: "session", id: `legion-daemon:${project}` };
const outbox = "\n\n<!-- legion-outbox: x -->";

// An issue's events: a message its author deleted, a standing note of the daemon's (its none-note,
// the daemon's refusal to admit an issue whose repository resolves to none), and the issue's close.
// In either order a filter gets them in, the deleted message comes ahead of the standing one, so a
// filter that read a body before it asked deleted_at would stop on it before a standing match
// could end its search.
const deleted: Event = {
  seq: 12,
  type: "message.created",
  actor: daemon,
  payload: { id: "m-deleted", deleted_at: "2026-11-21T10:50:00Z", deleted_by: daemon },
};
const standing: Event = daemonActorEvent(project, 11, `${noneNote}${outbox}`);
const closed: Event = {
  seq: 10,
  type: "issue.closed",
  actor: daemon,
  payload: { status: "done" },
};
/** The events newest first (seq 12, 11, 10), as multi-repo-acceptance.sh reads them. */
const newestFirst = [deleted, standing, closed];
// The same events oldest first (seq 10, 11, 12), as lib/workflow.sh's dispatch_events reads them:
// the deleted message takes the lowest seq, so it still comes ahead of the standing one, and the
// close the highest.
const deletedFirst: Event = { ...deleted, seq: 10 };
const closedLast: Event = { ...closed, seq: 12 };
const oldestFirst = [deletedFirst, standing, closedLast];
/** The standing message, saying BODY. */
const says = (body: string): Event => ({ ...standing, payload: { ...standing.payload, body } });

// multi-repo-acceptance.sh's note for a repository edit the daemon refused: the baseline's
// repository kept, and Dispatch's spelling of the one it refused.
const keptNote =
  "Legion keeps acme/smoke-t as this issue's repository; the change to acme/smoke-x in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for acme/smoke-x instead.";

describe("multi-repo-none-note.jq (multi-repo-acceptance.sh's negative no-repo)", () => {
  const args = { actor: daemon.id, none: noneNote };

  test("passes over the newest message, deleted: the standing none-note is the latest note", () => {
    expect(judge("multi-repo-none-note", args, newestFirst)).toBe(true);
  });

  test("beside the deleted message, a standing kept-repository note as the latest is no none-note", () => {
    expect(
      judge("multi-repo-none-note", args, [deleted, says(`${keptNote}${outbox}`), closed])
    ).toBe(false);
  });
});

describe("multi-repo-kept-note.jq (multi-repo-acceptance.sh's negative repo-edit)", () => {
  const args = { actor: daemon.id, note: keptNote };

  test("passes over the deleted message: no standing message is the kept-repository note", () => {
    expect(judge("multi-repo-kept-note", args, newestFirst)).toBe(false);
  });

  test("finds the standing kept-repository note beside the deleted message", () => {
    expect(
      judge("multi-repo-kept-note", args, [deleted, says(`${keptNote}${outbox}`), closed])
    ).toBe(true);
  });
});

describe("review-cap-posted.jq (lib/workflow.sh's review_cap_posted)", () => {
  test("passes over the deleted message: no standing message names review_round_cap=3", () => {
    expect(judge("review-cap-posted", {}, oldestFirst)).toBe(false);
  });

  test("finds the daemon's standing review_round_cap=3 message beside the deleted message", () => {
    const capped = says(
      `Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted.${outbox}`
    );
    expect(judge("review-cap-posted", {}, [deletedFirst, capped, closedLast])).toBe(true);
  });
});

describe("stage3-no-change-close.jq (stage3-devbox-workflow.sh's architect-closes-a-no-change-root)", () => {
  // The daemon's message for a close_root (internal/intake/facts.go), with Stage 3's reason.
  const reason = says(
    `Closed by its architect before its first phase: No change is needed: acme/smoke already has what this issue asks for. Close this issue without a change.${outbox}`
  );

  test("passes over the deleted message: no standing message is the close's reason", () => {
    expect(judge("stage3-no-change-close", {}, oldestFirst)).toBe(false);
  });

  test("finds the standing reason posted before the close beside the deleted message", () => {
    expect(judge("stage3-no-change-close", {}, [deletedFirst, reason, closedLast])).toBe(true);
  });

  test("a standing reason posted after the close is not before it", () => {
    expect(judge("stage3-no-change-close", {}, [{ ...closed, seq: 9 }, deletedFirst, reason])).toBe(
      false
    );
  });
});

describe("stage4b-report-posted.jq (stage4b-sandbox-tree.sh's report_posted)", () => {
  // The controller posts the report as its own session ($controller_session, the daemon state's
  // controllerLocator.sessionId), so here the fixture's two messages are the controller's.
  const controller = { kind: "session", id: "019aa2c4-5d3e-7f10-8b6a-2c9e4f1d7a35" };
  const byController = (list: readonly Event[]) =>
    list.map((event) =>
      event.type === "message.created" ? { ...event, actor: controller } : event
    );
  const args = { s: controller.id, tree: "LEGSMOKE-41" };

  test("passes over the deleted message: no standing message is the report", () => {
    expect(judge("stage4b-report-posted", args, byController(oldestFirst))).toBe(false);
  });

  test("finds the controller's standing report naming the tree and the free slots beside the deleted message", () => {
    const report = says(
      "Daily report for LEGSMOKE: LEGSMOKE-41 is implementing; 2 of 4 slots free."
    );
    expect(
      judge("stage4b-report-posted", args, byController([deletedFirst, report, closedLast]))
    ).toBe(true);
  });
});
