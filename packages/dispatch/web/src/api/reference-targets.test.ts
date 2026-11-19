import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { api, REFERENCE_TARGETS_MAX, REFERENCE_TARGETS_MAX_QUERY_CHARS } from "./client";
import { referenceTargetsQuery } from "./queries";

/**
 * The one batched read every name a page draws comes from (`referenceTargetsQuery`). The server
 * refuses a request naming more than `REFERENCE_TARGETS_MAX` references with `400 CAP_EXCEEDED`
 * (`packages/envoy/internal/dispatch/api/reference_targets.go`), and a page that names one
 * address per comment, message, ask and typed block of a long conversation can pass that cap.
 * The other bound is the client's budget for the request line
 * (`REFERENCE_TARGETS_MAX_QUERY_CHARS`), kept under a limit a proxy in front of Dispatch can
 * set, which a full batch of comment or message addresses passes. The reads they cost are what
 * this pins: one request for a set inside both, and a batch per bound above them, with every
 * address named in exactly one of them.
 */

/** `count` distinct block addresses, numbered so their sorted order is not their written one. */
function addresses(count: number): string[] {
  return Array.from({ length: count }, (_, index) => `dispatch://CORE-1/spec#b-r${index}`);
}

/** `count` distinct addresses of the length a conversation's own carry: a comment's id is a
 *  UUID, and the block inside it is another, so one address is 113 characters encoded, 118 with
 *  its `ref=` and the `&` before it. */
function longAddresses(count: number): string[] {
  return Array.from(
    { length: count },
    (_, index) =>
      `dispatch://CORE-1/comment/3f1c8a2e-5b77-4e21-9a4d-${String(index).padStart(12, "0")}#b-7c2d9f14-61ab-4f03-8d55-2b6e0a9c7e31`
  );
}

/** The route's own answer: one member per reference, in the request's order. */
function answerFor(refs: readonly string[]) {
  return {
    targets: refs.map((ref) => ({ gone: false, kind: "block", name: `words of ${ref}`, ref })),
  };
}

test("a set under the cap is one request, and the answer is the route's own", async () => {
  const read = spyOn(api, "getReferenceTargets").mockImplementation((refs) =>
    Promise.resolve(answerFor(refs))
  );
  try {
    const refs = addresses(REFERENCE_TARGETS_MAX);
    const answer = await new QueryClient().fetchQuery(referenceTargetsQuery(refs));
    expect(read.mock.calls.length).toBe(1);
    expect(answer.targets.map((target) => target.ref)).toEqual([...refs].sort());
  } finally {
    read.mockRestore();
  }
});

test("a set past the cap is read in batches of it, and every address is named once", async () => {
  const read = spyOn(api, "getReferenceTargets").mockImplementation((refs) =>
    Promise.resolve(answerFor(refs))
  );
  try {
    const refs = addresses(REFERENCE_TARGETS_MAX * 2 + 1);
    const answer = await new QueryClient().fetchQuery(referenceTargetsQuery(refs));
    // One request per batch of the cap, none of them past it: a batch the server refuses names
    // nothing at all.
    expect(read.mock.calls.map((call) => call[0].length)).toEqual([
      REFERENCE_TARGETS_MAX,
      REFERENCE_TARGETS_MAX,
      1,
    ]);
    // Every address the page asked for is answered, once, and the batches are one answer in the
    // key's own order, so a reader of the answer reads it as it reads an unbatched one.
    expect(answer.targets.map((target) => target.ref)).toEqual([...refs].sort());
    expect(new Set(answer.targets.map((target) => target.name)).size).toBe(refs.length);
  } finally {
    read.mockRestore();
  }
});

// A batch inside the server's cap can be a request line longer than a proxy in front of Dispatch
// admits. The server measured 247 references on its heaviest issue at about 17 KB of query string
// (the comment on `referenceTargetsMax`), past 16 KB, an AWS load balancer's limit; each of
// `longAddresses` writes 118 characters into the query string, so a cap's worth of them is about
// 30 KB. Refused there, the read would name none of the batch's references, so the length is a
// bound of its own, and a batch may write exactly the budget and no more.
test("a set inside the cap but past the request line's budget is read in batches of the budget", async () => {
  const read = spyOn(api, "getReferenceTargets").mockImplementation((refs) =>
    Promise.resolve(answerFor(refs))
  );
  try {
    const refs = longAddresses(REFERENCE_TARGETS_MAX);
    const answer = await new QueryClient().fetchQuery(referenceTargetsQuery(refs));
    const batches = read.mock.calls.map((call) => call[0]);
    expect(batches.length).toBeGreaterThan(1);
    for (const batch of batches) {
      expect(batch.length).toBeLessThanOrEqual(REFERENCE_TARGETS_MAX);
      const query = new URLSearchParams(batch.map((ref) => ["ref", ref])).toString();
      expect(
        query.length,
        `a batch of ${batch.length} addresses writes ${query.length} UTF-16 code units of query string`
      ).toBeLessThanOrEqual(REFERENCE_TARGETS_MAX_QUERY_CHARS);
    }
    // Nothing is lost to the second bound either: every address is asked for, once.
    expect(answer.targets.map((target) => target.ref)).toEqual([...refs].sort());
    expect(batches.flat().length).toBe(refs.length);

    // An address longer than the whole budget goes out alone, in a batch of its own, and the
    // addresses before it in sorted order still share one.
    read.mockClear();
    const long = `dispatch://CORE-1/spec#b-r2${"x".repeat(REFERENCE_TARGETS_MAX_QUERY_CHARS + 1)}`;
    const short = ["dispatch://CORE-1/spec#b-r0", "dispatch://CORE-1/spec#b-r1"];
    const mixed = await new QueryClient().fetchQuery(referenceTargetsQuery([long, ...short]));
    expect(read.mock.calls.map((call) => call[0])).toEqual([short, [long]]);
    expect(mixed.targets.map((target) => target.ref)).toEqual([...short, long]);

    // The same holds when the long address sorts first: it goes out alone, and the address
    // after it opens a batch of its own rather than an empty one before it.
    read.mockClear();
    const first = `dispatch://CORE-1/spec#b-a${"x".repeat(REFERENCE_TARGETS_MAX_QUERY_CHARS + 1)}`;
    const after = "dispatch://CORE-1/spec#b-r0";
    await new QueryClient().fetchQuery(referenceTargetsQuery([after, first]));
    expect(read.mock.calls.map((call) => call[0])).toEqual([[first], [after]]);

    // A batch whose query string is exactly the budget is one request, and one character more
    // is two: the `&` is counted only between two addresses, as URLSearchParams writes it.
    const queryOf = (batch: readonly string[]) =>
      new URLSearchParams(batch.map((ref) => ["ref", ref])).toString();
    const head = "dispatch://CORE-1/spec#b-r0";
    const tail = "dispatch://CORE-1/spec#b-r1";
    const fill = REFERENCE_TARGETS_MAX_QUERY_CHARS - queryOf([head, tail]).length;
    const exact = [`${head}${"x".repeat(fill)}`, tail];
    expect(queryOf(exact).length).toBe(REFERENCE_TARGETS_MAX_QUERY_CHARS);
    read.mockClear();
    await new QueryClient().fetchQuery(referenceTargetsQuery(exact));
    expect(read.mock.calls.map((call) => call[0])).toEqual([exact]);
    const over = [`${head}${"x".repeat(fill + 1)}`, tail];
    expect(queryOf(over).length).toBe(REFERENCE_TARGETS_MAX_QUERY_CHARS + 1);
    read.mockClear();
    await new QueryClient().fetchQuery(referenceTargetsQuery(over));
    expect(read.mock.calls.map((call) => call[0])).toEqual([[over[0]], [tail]]);
  } finally {
    read.mockRestore();
  }
});

test("the key holds every address the read asked for, without repeats and sorted", () => {
  const refs = [...addresses(REFERENCE_TARGETS_MAX + 10), "dispatch://CORE-1/spec#b-r0"];
  const { queryKey } = referenceTargetsQuery(refs);
  expect(queryKey[0]).toBe("reference-targets");
  expect(queryKey[1]).toEqual([...new Set(refs)].sort());
});

// A page that names no reference sends no request: a read of nothing still costs one, so the
// query is off, and even a fetch of it, which does not read `enabled`, calls the route zero times.
test("an empty set is not read", async () => {
  const read = spyOn(api, "getReferenceTargets").mockImplementation((refs) =>
    Promise.resolve(answerFor(refs))
  );
  try {
    const options = referenceTargetsQuery([]);
    expect(options.enabled).toBe(false);
    expect(options.queryKey[0]).toBe("reference-targets");
    expect(options.queryKey[1]).toEqual([]);
    const answer = await new QueryClient().fetchQuery(options);
    expect(read.mock.calls.length).toBe(0);
    expect(answer.targets).toEqual([]);
  } finally {
    read.mockRestore();
  }
});
