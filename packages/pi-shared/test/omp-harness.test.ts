import { expect, test } from "bun:test";
import { deliverSubject, RelayHeader } from "@legion/envoy-client/consumer-delivery";
import { until } from "@legion/test-fixtures/wait";
import {
  isSelfCheck,
  newestMessage,
  ownMessages,
  type RelayFrame,
  STANDIN_STREAM_ID,
  sendRelayFrame,
  transcriptRecordsIn,
} from "./omp-harness";

// The host's date/cwd reminder as the pinned Oh My Pi sent it in the stall watch's live proof, with
// the pane's local date moved on between two requests (the dump of that run, 2026-11-22).
const REMINDER =
  "<system-reminder>\nToday: 2026-11-23; current working directory: '/scratch/workspace'. Do not repeat this information in your reply.\n</system-reminder>";

const RESTART =
  "Your turn was ended because its `eval` call (toolu_msg_2_0) had been running 13 seconds with no result.";

/** A Messages request whose messages are `messages`. */
function body(...messages: unknown[]): Record<string, unknown> {
  return { messages };
}

test("the reminder sent as a message of its own after the newest message is not the newest message", () => {
  // The restart turn's request in that run: the agent's own message, then the reminder alone.
  const request = body(
    { role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_1", content: "ok" }] },
    { role: "user", content: [{ type: "text", text: RESTART }] },
    {
      role: "user",
      content: [{ type: "text", text: REMINDER, cache_control: { type: "ephemeral" } }],
    }
  );
  expect(newestMessage(request)).toContain("Your turn was ended because");
  expect(ownMessages(request)).toHaveLength(2);
});

test("the reminder ahead of a user turn is taken off, in each form the host sends it", () => {
  // Joined into one text, as the host sends a text-only model a message's blocks.
  expect(
    ownMessages(
      body({ role: "user", content: [{ type: "text", text: `${REMINDER}\nSecond question.` }] })
    )
  ).toEqual([{ role: "user", content: [{ type: "text", text: "Second question." }] }]);
  // A string content, prefixed with a blank line.
  expect(ownMessages(body({ role: "user", content: `${REMINDER}\n\nSecond question.` }))).toEqual([
    { role: "user", content: "Second question." },
  ]);
  // A text block of its own ahead of the turn's blocks.
  expect(
    ownMessages(
      body({
        role: "user",
        content: [
          { type: "text", text: REMINDER },
          { type: "text", text: "Second question." },
        ],
      })
    )
  ).toEqual([{ role: "user", content: [{ type: "text", text: "Second question." }] }]);
});

test("a message that only quotes a reminder, or carries none, is left as it is", () => {
  const quoted = { role: "user", content: [{ type: "text", text: `Look: ${REMINDER}` }] };
  const plain = { role: "assistant", content: [{ type: "text", text: "Done." }] };
  expect(ownMessages(body(quoted, plain))).toEqual([quoted, plain]);
  expect(newestMessage(body())).toBe("null");
});

test("a side question is a side question when the host sends its reminder after it", () => {
  const question = {
    role: "user",
    content: [{ type: "text", text: "<btw>\nWhat is up?\n</btw>" }],
  };
  const reminder = { role: "user", content: [{ type: "text", text: REMINDER }] };
  const turn = { role: "user", content: [{ type: "text", text: "Go on." }] };
  const path = "/anthropic/v1/messages";
  expect(isSelfCheck({ path, body: body(question, reminder) })).toBe(true);
  // A turn after a side question's exchange is no side question.
  expect(isSelfCheck({ path, body: body(question, turn, reminder) })).toBe(false);
});

test("a transcript's entries of one custom type carry the moment the host wrote each", () => {
  const entries = [
    { type: "custom", customType: "probe", timestamp: "2026-11-28T10:00:00.000Z", data: { n: 1 } },
    { type: "message", timestamp: "2026-11-28T10:00:00.500Z" },
    { type: "custom", customType: "other", timestamp: "2026-11-28T10:00:01.000Z", data: 2 },
    { type: "custom", customType: "probe", timestamp: "2026-11-28T10:00:02.250Z" },
  ];
  expect(transcriptRecordsIn(entries, "probe")).toEqual([
    { at: Date.parse("2026-11-28T10:00:00.000Z"), data: { n: 1 } },
    { at: Date.parse("2026-11-28T10:00:02.250Z"), data: undefined },
  ]);
  // An entry of the type with no moment of its own is no record of when the host acted.
  expect(() =>
    transcriptRecordsIn([{ type: "custom", customType: "probe", data: 3 }], "probe")
  ).toThrow("a probe entry carries no timestamp");
});

/**
 * A relay frame over a NATS connection whose `answer` makes the outcome of each request, numbered
 * from 1, recording each request's subject and timeout and the header set it was sent with. Each
 * ask of the send's wait reads `stopped` once (`asks`), and `end` makes the run's end that read.
 */
function relayRig(answer: (attempt: number) => Promise<{ readonly data: Uint8Array }>) {
  const sent: { readonly subject: string; readonly timeout: number | undefined }[] = [];
  const headerSets: Map<string, string>[] = [];
  let asks = 0;
  let ended: unknown;
  const frame: RelayFrame = {
    connection: {
      request: (subject, _payload, options) => {
        sent.push({ subject, timeout: options?.timeout });
        return answer(sent.length);
      },
    },
    newHeaders: () => {
      const headers = new Map<string, string>();
      headerSets.push(headers);
      return headers;
    },
    sessionID: "ses_relay",
    subject: "notifications.agent.ses_relay",
    payload: new TextEncoder().encode("{}"),
    seq: 7,
    storedAt: "2026-11-28T10:00:00.000000000Z",
    stopped: {
      get error() {
        asks += 1;
        return ended;
      },
    },
  };
  return {
    frame,
    sent,
    headerSets,
    asks: () => asks,
    end: (error: unknown) => {
      ended = error;
    },
  };
}

/** nats.js's answer to a request no one has subscribed for: no responders, its code "503". */
function noResponders(): Promise<never> {
  return Promise.reject(Object.assign(new Error("503"), { code: "503" }));
}

test("a frame goes again, as the one frame it is, until the session has subscribed, and the send answers its take", async () => {
  const rig = relayRig((attempt) =>
    attempt < 3 ? noResponders() : Promise.resolve({ data: new TextEncoder().encode("received") })
  );
  expect(await sendRelayFrame(rig.frame)).toBe("received");

  expect(rig.sent.map(({ subject }) => subject)).toEqual(
    Array(3).fill(deliverSubject("ses_relay"))
  );
  // Each request may wait as long as the row has left, in the whole milliseconds nats.js takes.
  for (const { timeout } of rig.sent) {
    expect(Number.isInteger(timeout) && (timeout ?? 0) >= 1, `timeout ${timeout}`).toBe(true);
  }
  expect(rig.headerSets.map((headers) => Object.fromEntries(headers))).toEqual(
    Array(3).fill({
      [RelayHeader.subject]: "notifications.agent.ses_relay",
      [RelayHeader.streamSeq]: "7",
      [RelayHeader.streamID]: STANDIN_STREAM_ID,
      [RelayHeader.delivered]: "1",
      [RelayHeader.storedAt]: "2026-11-28T10:00:00.000000000Z",
    })
  );
});

test("a run that ended fails the send at once, while its request still waits", async () => {
  const rig = relayRig(() => new Promise(() => {}));
  const sending = sendRelayFrame(rig.frame);
  await until("the send to make its request", () => rig.sent.length === 1);

  const ended = new Error("omp exited");
  rig.end(ended);
  await expect(sending).rejects.toBe(ended);
  expect(rig.sent).toHaveLength(1);
});

// Each request waits as long as the row has left, so nats.js ending one at its own timeout
// (`code: "TIMEOUT"`) is the row's deadline arriving, and the frame is not sent again.
test("a request that ended at its own timeout is not sent again", async () => {
  const rig = relayRig(() =>
    Promise.reject(Object.assign(new Error("TIMEOUT"), { code: "TIMEOUT" }))
  );
  const sending = sendRelayFrame(rig.frame);
  // The first ask sends; the request's answer settles before the wait's next ask, so the asks
  // after it each found the timeout and sent nothing.
  await until("the send's wait to ask twice after the ask that sent", () => rig.asks() >= 3);

  const ended = new Error("omp exited");
  rig.end(ended);
  await expect(sending).rejects.toBe(ended);
  expect(rig.sent).toHaveLength(1);
});
