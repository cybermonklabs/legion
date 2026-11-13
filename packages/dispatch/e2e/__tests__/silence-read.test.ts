import { expect, test } from "bun:test";

import { readUntilSilent, type SilenceRead, type Timers } from "../silence-read";

// The probe's `--compare` read, on a clock these rows move by hand and a GET they answer piece by
// piece, so each row reads where the deadline starts over without spending real time on it.

const route = "/api/v1/delivery/timeline?from=a&to=b";
const withinMs = 3_000;
const silent = `the read of ${route} received nothing for 3 s (--read-within)`;

/** Timers on a clock that moves only when `advance` moves it. */
interface HandClock {
  readonly timers: Timers;
  /** Moves the clock on by `ms` and fires each timer whose time has come, earliest first. */
  advance(ms: number): void;
  /** The time the clock has moved, in milliseconds. */
  elapsed(): number;
  /** How many timers are still set. */
  pending(): number;
}

function handClock(): HandClock {
  let now = 0;
  let last = 0;
  const due = new Map<number, { readonly at: number; readonly fire: () => void }>();
  return {
    advance(ms) {
      now += ms;
      const ripe = [...due].filter(([, timer]) => timer.at <= now);
      for (const [handle, timer] of ripe.sort(([, a], [, b]) => a.at - b.at)) {
        if (due.delete(handle)) timer.fire();
      }
    },
    elapsed: () => now,
    pending: () => due.size,
    timers: {
      clearTimeout: (handle) => {
        due.delete(handle as number);
      },
      setTimeout: (fire, ms) => {
        last += 1;
        due.set(last, { at: now + ms, fire });
        return last;
      },
    },
  };
}

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

function startRead(get: HandGet, clock: HandClock): Reading {
  const reading = readUntilSilent(route, (signal) => get.request(signal), withinMs, clock.timers);
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
  const clock = handClock();
  const get = handGet();
  const read = startRead(get, clock);
  const pieces = ['{"prs":', "[1,", "2,", "3]}"];

  // Each wait stops one millisecond short of the deadline, and the read's whole length passes it.
  clock.advance(withinMs - 1);
  get.answer();
  for (const piece of pieces) {
    const give = await read.asked();
    clock.advance(withinMs - 1);
    give(piece);
  }
  const end = await read.asked();
  clock.advance(withinMs - 1);
  end(null);

  const { response, text } = await read.reading;
  expect(response.status).toBe(200);
  expect(text).toBe(pieces.join(""));
  expect(clock.elapsed()).toBeGreaterThan(withinMs);
  expect(clock.pending()).toBe(0);
});

test("a read whose body falls silent for the deadline fails, naming its route", async () => {
  const clock = handClock();
  const get = handGet();
  const read = startRead(get, clock);

  get.answer();
  const give = await read.asked();
  give('{"prs":');
  await read.asked();
  clock.advance(withinMs);

  await expect(read.reading).rejects.toThrow(silent);
  expect(clock.pending()).toBe(0);
});

test("a read whose headers never come fails at the deadline, naming its route", async () => {
  const clock = handClock();
  const get = handGet();
  const read = startRead(get, clock);

  clock.advance(withinMs);

  await expect(read.reading).rejects.toThrow(silent);
  expect(clock.pending()).toBe(0);
});
