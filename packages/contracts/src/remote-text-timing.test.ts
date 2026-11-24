import { expect, test } from "bun:test";

// Another party writes the field, so a page or an answer can nest one reference as deep as its
// length allows: `&amp;amp;amp;…` or `&ampampamp…;`, which a decoding until no reference is left
// reads one level per pass, each pass the field's whole length, a time quadratic in it. The rule's
// decoding (readRemoteText's step 0) makes at most REFERENCE_PASSES + 1 passes, so a field of up
// to 1 MiB, the longest the rule reads (FIELD_LIMIT), reads within READ_BOUND_MS and is not
// readable; a longer field the rule refuses before any step reads it. Each read runs in a worker
// of its own, which the test ends when the bound passes, so a read past the bound fails at the
// bound. Go's TestNestedReferencesAreReadInBoundedTime holds redact.Remote and
// redact.UnescapeHTML to the same, and TestAFieldLongerThanTheLimitIsRefusedInBoundedTime holds
// redact.Remote to the limit.

/** The rule's module, which each worker loads. */
const RULE = new URL("./remote-text.ts", import.meta.url).href;

/** The length of each hostile field: the review timed 64 KiB and 32 KiB. */
const SIZE = 1 << 20;

/** How long one read of a hostile field may take. At a load average of 130 to 180 on 288 CPUs
 *  (this box runs at 100 to 1200) the fixed read took at most 16.5 ms, and the bound is 300 times
 *  that. Decoding until no reference was left, as the rule did before, made a pass for each level
 *  the field nests: at a load average of 147 to 265 it took 74.4 s on the `&amp;amp;amp;…` field
 *  and 352.7 s on the `&ampampamp…;` one, so each fails at the bound rather than waiting that
 *  out. */
const READ_BOUND_MS = 5_000;

/** What a read answers when the bound passes before it does. */
const PAST_THE_BOUND = Symbol("past the bound");

/** A worker that loads the rule, says so, then reads each field it is sent and answers what
 *  readRemoteText made of it. */
function ruleWorker(): Worker {
  const source = `import { readRemoteText } from ${JSON.stringify(RULE)};
self.onmessage = (event) => postMessage({ read: readRemoteText(event.data) ?? null });
postMessage("ready");`;
  return new Worker(URL.createObjectURL(new Blob([source], { type: "text/javascript" })));
}

/** The next message `worker` sends; with `boundMs`, PAST_THE_BOUND where that passes first. The
 *  timer is the platform clock on purpose: the time a read takes is what this test holds, so a
 *  fake clock could not see it; it waits no longer than the read does, and only past the bound. */
function nextMessage(worker: Worker, boundMs?: number): Promise<unknown> {
  const { promise, resolve, reject } = Promise.withResolvers<unknown>();
  const timer =
    boundMs === undefined ? undefined : setTimeout(() => resolve(PAST_THE_BOUND), boundMs);
  worker.onmessage = (event) => {
    clearTimeout(timer);
    resolve(event.data);
  };
  worker.onerror = (event) => {
    clearTimeout(timer);
    reject(event.error ?? new Error(event.message));
  };
  return promise;
}

/** Reads `field` in a worker of its own and holds the read to `boundMs`: the read must answer
 *  within it, and answer that the field is not readable. */
async function readsAsNotReadableWithin(field: string, boundMs: number): Promise<void> {
  const worker = ruleWorker();
  try {
    // Loading the module is not the read: the bound starts once the worker holds the rule.
    expect(await nextMessage(worker)).toBe("ready");
    const began = performance.now();
    const read = nextMessage(worker, boundMs);
    worker.postMessage(field);
    const answer = await read;
    console.log(`the read took ${(performance.now() - began).toFixed(1)} ms`);
    expect(answer, `the read did not answer within ${boundMs} ms`).not.toBe(PAST_THE_BOUND);
    expect(answer).toEqual({ read: null });
  } finally {
    worker.terminate();
  }
}

// Each field is one reference short of 1 MiB, so it is no longer than FIELD_LIMIT and the rule's
// passes read it.
test.each([
  ["&amp;amp;amp;…", `&${"amp;".repeat(SIZE / 4 - 1)}`],
  ["&ampampamp…;", `&${"amp".repeat(Math.floor((SIZE - 2) / 3))};`],
])(
  "readRemoteText of a 1 MiB field nested as %s reads it as not readable within the bound",
  (_shape, field) => readsAsNotReadableWithin(field, READ_BOUND_MS),
  60_000 + READ_BOUND_MS + 10_000
);

/** How long one read of a 256 KiB field that is one long reference may take. A reference's name
 *  is at most 32 letters (`CHARACTER_REFERENCE`), as in Go, where a name as long as the field cost
 *  each pass far more than the field's length (`TestALongReferenceNameIsReadInBoundedTime`). V8
 *  read the long name in linear time with no cap too: at a load average of 130 to 150 on 288 CPUs
 *  the read took 3.0 ms with no cap and 0.9 ms with it, so no bound tells the two apart on this
 *  box. This one, more than 250 times the read, holds the read to linear time. */
const LONG_NAME_BOUND_MS = 250;

test(
  "readRemoteText of a 256 KiB field that is one long reference (&ampampamp…;) reads it as not readable within the bound",
  () =>
    readsAsNotReadableWithin(`&${"amp".repeat(Math.floor((256 << 10) / 3))};`, LONG_NAME_BOUND_MS),
  60_000 + LONG_NAME_BOUND_MS + 10_000
);

/** How many times a worker of {@link timedRuleWorker} reads the field it is sent, and how long the
 *  least of those reads may take: the least is the read's own cost, and what a busy box adds to one
 *  read is not. */
const LONG_FIELD_READS = 3;
const LONG_FIELD_BOUND_MS = 20;

/** A worker that loads the rule, says so, then reads each field it is sent LONG_FIELD_READS times
 *  and answers what readRemoteText made of it and the least time one read took, so the time a
 *  field takes to reach the worker is not counted. */
function timedRuleWorker(): Worker {
  const source = `import { readRemoteText } from ${JSON.stringify(RULE)};
self.onmessage = (event) => {
  let least = Number.POSITIVE_INFINITY;
  let read;
  for (let i = 0; i < ${LONG_FIELD_READS}; i++) {
    const began = performance.now();
    read = readRemoteText(event.data);
    least = Math.min(least, performance.now() - began);
  }
  postMessage({ read: read ?? null, least });
};
postMessage("ready");`;
  return new Worker(URL.createObjectURL(new Blob([source], { type: "text/javascript" })));
}

/** The length of each field longer than the limit: githubrest reads answers of up to 64 MiB. */
const LONG_FIELD = 64 << 20;

// The rule reads no field longer than FIELD_LIMIT, so its read of a 64 MiB field of either shape
// that costs it most per byte, one long numeric reference or one long name, stops at the limit.
// Before the limit, at a load average of 110 to 130 on 288 CPUs, the least of three reads of these
// fields took 1.16 s and 0.40 s, past the bound.
test.each([
  [
    "one long numeric reference (&amp;amp;amp;amp;amp;&#000…01;)",
    `&${"amp;".repeat(5)}&#${"0".repeat(LONG_FIELD)}1;`,
  ],
  ["one long name (&ampampamp…;)", `&${"amp".repeat(Math.floor(LONG_FIELD / 3))};`],
])(
  "readRemoteText of a 64 MiB field that is %s refuses it within the bound",
  async (_shape, field) => {
    const worker = timedRuleWorker();
    try {
      expect(await nextMessage(worker)).toBe("ready");
      const read = nextMessage(worker, READ_BOUND_MS);
      worker.postMessage(field);
      const answer = await read;
      expect(answer, `the reads did not answer within ${READ_BOUND_MS} ms`).not.toBe(
        PAST_THE_BOUND
      );
      const { read: reading, least } = answer as { read: string | null; least: number };
      console.log(`the least of ${LONG_FIELD_READS} reads took ${least.toFixed(3)} ms`);
      expect(reading).toBeNull();
      expect(least).toBeLessThanOrEqual(LONG_FIELD_BOUND_MS);
    } finally {
      worker.terminate();
    }
  },
  60_000 + READ_BOUND_MS + 10_000
);
