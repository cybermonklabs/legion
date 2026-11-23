import { expect, test } from "bun:test";

// Another party writes the field, so a page or an answer can nest one reference as deep as its
// length allows: `&amp;amp;amp;…` or `&ampampamp…;`, which a decoding until no reference is left
// reads one level per pass, each pass the field's whole length, a time quadratic in it. The rule's
// decoding (readRemoteText's step 0) makes at most REFERENCE_PASSES + 1 passes, so a 1 MiB field
// reads within READ_BOUND_MS and is not readable. Each read runs in a worker of its own, which the
// test ends when the bound passes, so a read past the bound fails at the bound. Go's
// TestNestedReferencesAreReadInBoundedTime holds redact.Remote and redact.UnescapeHTML to the same.

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

test.each([
  ["&amp;amp;amp;…", `&${"amp;".repeat(SIZE / 4)}`],
  ["&ampampamp…;", `&${"amp".repeat(Math.floor(SIZE / 3))};`],
])(
  "readRemoteText of a 1 MiB field nested as %s reads it as not readable within the bound",
  async (_shape, field) => {
    const worker = ruleWorker();
    try {
      // Loading the module is not the read: the bound starts once the worker holds the rule.
      expect(await nextMessage(worker)).toBe("ready");
      const began = performance.now();
      const read = nextMessage(worker, READ_BOUND_MS);
      worker.postMessage(field);
      const answer = await read;
      console.log(`the read took ${(performance.now() - began).toFixed(1)} ms`);
      expect(answer, `the read did not answer within ${READ_BOUND_MS} ms`).not.toBe(PAST_THE_BOUND);
      expect(answer).toEqual({ read: null });
    } finally {
      worker.terminate();
    }
  },
  60_000 + READ_BOUND_MS + 10_000
);
