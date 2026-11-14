import { expect, test } from "bun:test";

import { eventStreamDecoder, readEventStream } from "./event-stream";

function decodeAll(pieces: readonly string[]): unknown[] {
  const events: unknown[] = [];
  const decode = eventStreamDecoder((event) => events.push(event));
  for (const piece of pieces) decode(piece);
  decode("", true);
  return events;
}

/** Dispatch's answer stream as `writeGoingFrame` writes it. */
const frames =
  'id: 1\nevent: text\ndata: {"text":"Two "}\n\n' +
  'id: 2\nevent: item\ndata: {"kind":"issue","id":"DEMO-1","href":"/issues/DEMO-1","title":"t"}\n\n' +
  'id: 3\nevent: end\ndata: {"words":2,"items":1}\n\n';

const expected = [
  { data: '{"text":"Two "}', id: "1", name: "text" },
  {
    data: '{"kind":"issue","id":"DEMO-1","href":"/issues/DEMO-1","title":"t"}',
    id: "2",
    name: "item",
  },
  { data: '{"words":2,"items":1}', id: "3", name: "end" },
];

test("frames split anywhere across the pieces the network delivers decode as the whole stream does", () => {
  expect(decodeAll([frames])).toEqual(expected);
  // Every split point: a frame cut inside its field name, its JSON, between its two newlines.
  for (let at = 1; at < frames.length; at += 1) {
    expect(decodeAll([frames.slice(0, at), frames.slice(at)])).toEqual(expected);
  }
  expect(decodeAll(Array.from(frames))).toEqual(expected);
});

test("CRLF and CR line breaks, a CRLF split across two pieces included, are one break each", () => {
  const crlf = frames.replaceAll("\n", "\r\n");
  for (let at = 1; at < crlf.length; at += 1) {
    expect(decodeAll([crlf.slice(0, at), crlf.slice(at)])).toEqual(expected);
  }
  expect(decodeAll([frames.replaceAll("\n", "\r")])).toEqual(expected);
});

test("comments are skipped, data lines join with a newline, and an id holds until replaced", () => {
  expect(
    decodeAll([": keepalive\n\nid: 7\ndata: one\ndata: two\n\nevent: text\ndata:three\n\n"])
  ).toEqual([
    { data: "one\ntwo", id: "7", name: "message" },
    { data: "three", id: "7", name: "text" },
  ]);
});

test("an event cut off by the end of the stream is not dispatched", () => {
  expect(decodeAll(['id: 1\nevent: text\ndata: {"text":"a"}\n'])).toEqual([]);
});

function bodyOf(pieces: readonly string[]): ReadableStream<Uint8Array> {
  const bytes = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const piece of pieces) controller.enqueue(bytes.encode(piece));
      controller.close();
    },
  });
}

test("reading stops at the event the reader ends on, and reports whether it ended or ran out", async () => {
  const seen: string[] = [];
  const stopped = await readEventStream(
    bodyOf([frames, "id: 4\nevent: text\ndata: {}\n\n"]),
    (event) => {
      seen.push(event.name);
      return event.name === "end";
    }
  );
  expect(stopped).toBe(true);
  expect(seen).toEqual(["text", "item", "end"]);

  const ranOut = await readEventStream(bodyOf([frames.slice(0, 40)]), () => false);
  expect(ranOut).toBe(false);
});

test("an event handler that throws cancels the body it was reading, which is still open", async () => {
  let cancelled = false;
  const body = new ReadableStream<Uint8Array>({
    cancel() {
      cancelled = true;
    },
    start(controller) {
      // A live stream: one frame in, and the body neither ends nor fails.
      controller.enqueue(new TextEncoder().encode('id: 1\nevent: text\ndata: {"text":"a"}\n\n'));
    },
  });
  const thrown = new Error("a frame the reader could not take");
  const read = readEventStream(body, () => {
    throw thrown;
  });
  expect(await read.catch((error: unknown) => error)).toBe(thrown);
  expect(cancelled).toBe(true);

  // A body that ended is not cancelled: there is nothing left of it to close.
  let endedCancelled = false;
  const ended = new ReadableStream<Uint8Array>({
    cancel() {
      endedCancelled = true;
    },
    start(controller) {
      controller.enqueue(new TextEncoder().encode(frames));
      controller.close();
    },
  });
  expect(await readEventStream(ended, () => false)).toBe(false);
  expect(endedCancelled).toBe(false);
});

test("a multi-byte character split across two pieces of bytes decodes whole", async () => {
  const encoded = new TextEncoder().encode('data: {"text":"é"}\n\n');
  const split = encoded.indexOf(0xc3) + 1;
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(encoded.slice(0, split));
      controller.enqueue(encoded.slice(split));
      controller.close();
    },
  });
  const data: string[] = [];
  await readEventStream(body, (event) => {
    data.push(event.data);
    return false;
  });
  expect(data).toEqual(['{"text":"é"}']);
});
