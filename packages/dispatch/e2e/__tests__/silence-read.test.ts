import { afterEach, beforeEach, expect, jest, test } from "bun:test";

import { readUntilSilent, type SilenceRead } from "../silence-read";

// The probe's `--compare` read, on Bun's fake timers and a GET these rows answer piece by piece,
// so each row moves the deadline with `jest.advanceTimersByTime` and reads where it starts over
// without spending real time on it.

const route = "/api/v1/delivery/timeline?from=a&to=b";
const withinMs = 3_000;
const silent = `the read of ${route} received nothing for 3 s (--read-within)`;

beforeEach(() => {
  jest.useFakeTimers();
});

// A fake clock left behind stops the timers of every file the same worker runs after this one.
afterEach(() => {
  jest.useRealTimers();
});

/** Sends the next piece of a body, or ends it when given null. */
type Give = (piece: string | null) => void;

/** A GET answered by hand. An abort of the read fails the headers while they are still to come
 *  and the body while it is being read, as `fetch` does. */
interface HandGet {
  /** What the read calls in place of `fetch`. */
  request(signal: AbortSignal): Promise<Response>;
  /** Sends the headers. */
  answer(): void;
  /** Resolves once the read asks for the next piece of the body, which it does only after it has
   *  handled the piece before, with the call that sends that piece. */
  asked(): Promise<Give>;
}

function handGet(): HandGet {
  const encoder = new TextEncoder();
  const headers = Promise.withResolvers<Response>();
  let asking = Promise.withResolvers<Give>();
  let aborted = false;
  let stream: ReadableStreamDefaultController<Uint8Array> | undefined;
  const body = new ReadableStream<Uint8Array>(
    {
      pull(controller) {
        const given = Promise.withResolvers<void>();
        asking.resolve((piece) => {
          if (!aborted) {
            if (piece === null) controller.close();
            else controller.enqueue(encoder.encode(piece));
          }
          given.resolve();
        });
        return given.promise;
      },
      start(controller) {
        stream = controller;
      },
    },
    // No piece is pulled before the read asks for one.
    { highWaterMark: 0 }
  );
  return {
    answer: () => headers.resolve(new Response(body)),
    async asked() {
      const give = await asking.promise;
      asking = Promise.withResolvers();
      return give;
    },
    request(signal) {
      signal.addEventListener("abort", () => {
        aborted = true;
        headers.reject(signal.reason);
        stream?.error(signal.reason);
      });
      return headers.promise;
    },
  };
}

/** One read under way: the read itself, and the wait for its next ask, which fails with what the
 *  read rejected with when the read ends first. */
interface Reading {
  readonly reading: Promise<SilenceRead>;
  asked(): Promise<Give>;
}

function startRead(get: HandGet): Reading {
  const reading = readUntilSilent(route, (signal) => get.request(signal), withinMs);
  const ended = reading.then(
    () => new Error("the read ended before it asked for its next piece"),
    (error: unknown) => (error instanceof Error ? error : new Error(String(error)))
  );
  return {
    async asked() {
      const next = await Promise.race([get.asked(), ended]);
      if (next instanceof Error) throw next;
      return next;
    },
    reading,
  };
}

test("the deadline starts over on the headers and on each piece, so a long read is read whole", async () => {
  const get = handGet();
  const read = startRead(get);
  const pieces = ['{"prs":', "[1,", "2,", "3]}"];
  let moved = 0;
  const advance = (ms: number) => {
    jest.advanceTimersByTime(ms);
    moved += ms;
  };

  // Each wait stops one millisecond short of the deadline, and the read's whole length passes it.
  advance(withinMs - 1);
  get.answer();
  for (const piece of pieces) {
    const give = await read.asked();
    advance(withinMs - 1);
    give(piece);
  }
  const end = await read.asked();
  advance(withinMs - 1);
  end(null);

  const { response, text } = await read.reading;
  expect(response.status).toBe(200);
  expect(text).toBe(pieces.join(""));
  expect(moved).toBeGreaterThan(withinMs);
  expect(jest.getTimerCount()).toBe(0);
});

test("a read whose body falls silent for the deadline fails, naming its route", async () => {
  const get = handGet();
  const read = startRead(get);

  get.answer();
  const give = await read.asked();
  give('{"prs":');
  await read.asked();
  jest.advanceTimersByTime(withinMs);

  await expect(read.reading).rejects.toThrow(silent);
  expect(jest.getTimerCount()).toBe(0);
});

test("a read whose headers never come fails at the deadline, naming its route", async () => {
  const get = handGet();
  const read = startRead(get);

  jest.advanceTimersByTime(withinMs);

  await expect(read.reading).rejects.toThrow(silent);
  expect(jest.getTimerCount()).toBe(0);
});
