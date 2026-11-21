import { describe, expect, test } from "bun:test";
import { decode } from "@toon-format/toon";
import { type ConsumerGapNotice, GAP_NOTICE, gapNoticeText } from "../consumer-delivery";
import { closeReplayedRemoval } from "../delivery-ledger";

const ISSUE = "notifications.dispatch.issue.DSP-1.>";
const OTHER_ISSUE = "notifications.dispatch.issue.DSP-2.>";
/** When the stream stored the removal frame; the host's relay answers against it. */
const STORED_AT = Date.UTC(2026, 9, 26, 12, 0, 0);

describe("a stored subscription removal", () => {
  /** A Dispatch `subscription.removed` naming `session`, as the stream stores it. */
  const removal = (session: string, topics: readonly string[]) =>
    JSON.stringify({
      source: "dispatch",
      payload: JSON.stringify({
        issue_key: "DSP-1",
        type: "subscription.removed",
        actor: { kind: "user", id: "alice" },
        payload: { session_id: session, by: { kind: "user", id: "alice" }, topics },
      }),
    });

  /** A host whose relay followed each topic before the frame was stored (true) or after it. */
  const host = (followed: Record<string, boolean>) => {
    const open = new Set(Object.keys(followed));
    const calls: string[] = [];
    let unregistered = false;
    return {
      open,
      calls,
      unregisteredBeforeReturn: () => unregistered,
      removalHost: {
        sessionID: "ses-1",
        relay: { followedBefore: (topic: string) => followed[topic] === true },
        close: (topic: string) => open.delete(topic),
        unregister: async (topics: readonly string[]) => {
          calls.push(`unregister ${topics.join(" ")}`);
          await Promise.resolve();
          unregistered = true;
        },
        followedChanged: async () => {
          calls.push("followedChanged");
        },
      },
    };
  };

  test("closes only a topic followed before it was stored, and takes it out of the registry", async () => {
    const session = host({ [ISSUE]: true, [OTHER_ISSUE]: false });

    const closed = await closeReplayedRemoval(
      {
        data: removal("ses-1", [ISSUE, OTHER_ISSUE, "notifications.dispatch.issue.DSP-3.>"]),
        storedAtMs: STORED_AT,
      },
      session.removalHost
    );

    expect(closed).toEqual([ISSUE]);
    expect([...session.open]).toEqual([OTHER_ISSUE]);
    expect(session.calls).toEqual([`unregister ${ISSUE}`, "followedChanged"]);
    expect(session.unregisteredBeforeReturn()).toBe(true);
  });

  test("closes nothing for another session's removal, or one stored before the follow", async () => {
    const session = host({ [ISSUE]: true });
    const later = host({ [OTHER_ISSUE]: false });

    expect(
      await closeReplayedRemoval(
        { data: removal("ses-2", [ISSUE]), storedAtMs: STORED_AT },
        session.removalHost
      )
    ).toEqual([]);
    expect(
      await closeReplayedRemoval(
        { data: removal("ses-1", [OTHER_ISSUE]), storedAtMs: STORED_AT },
        later.removalHost
      )
    ).toEqual([]);
    expect(
      await closeReplayedRemoval(
        { data: JSON.stringify({ source: "dispatch" }), storedAtMs: STORED_AT },
        session.removalHost
      )
    ).toEqual([]);
    expect([...session.open]).toEqual([ISSUE]);
    expect([...later.open]).toEqual([OTHER_ISSUE]);
    expect(session.calls).toEqual([]);
  });
});

describe("a gap notice", () => {
  const card = (notice: ConsumerGapNotice) => decode(gapNoticeText(notice));

  // What the relay knows of the gap is a field each; the log's sentence stays out.
  test("is a card for the session that names the gap's facts", () => {
    expect(
      card({
        kind: "stream_recreated",
        stream: "stream-new",
        previousStream: "stream-old",
        message: "recreated",
      })
    ).toEqual({
      envoy: {
        notice: GAP_NOTICE.kind,
        stream: "stream-new",
        previous_stream: "stream-old",
      },
    });
    expect(
      card({ kind: "retention_gap", stream: undefined, retentionHours: 72, message: "too old" })
    ).toEqual({
      envoy: { notice: GAP_NOTICE.kind, retention_hours: 72 },
    });
  });

  // No field names the cause (R2 of dispatch://INFRA-3686: no field the same in every notice of
  // its kind), so each cause's notice states the field of its own cause and not the other's.
  test("tells its cause by its fields alone", () => {
    const retention = card({
      kind: "retention_gap",
      stream: "stream-7f3a",
      retentionHours: 72,
      message: "too old",
    });
    const recreated = card({
      kind: "stream_recreated",
      stream: "stream-new",
      previousStream: "stream-old",
      message: "recreated",
    });

    expect(retention).toHaveProperty("envoy.retention_hours");
    expect(retention).not.toHaveProperty("envoy.previous_stream");
    expect(recreated).toHaveProperty("envoy.previous_stream");
    expect(recreated).not.toHaveProperty("envoy.retention_hours");
    for (const told of [retention, recreated]) expect(told).not.toHaveProperty("envoy.cause");
  });
});
