import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { api, REFERENCE_TARGETS_MAX, REFERENCE_TARGETS_MAX_QUERY_CHARS } from "./client";
import { referenceTargetsQuery } from "./queries";

/**
 * The one batched read every name a page draws comes from (`referenceTargetsQuery`). The server
 * refuses a request naming more than `REFERENCE_TARGETS_MAX` references with `400 CAP_EXCEEDED`
 * (`packages/envoy/internal/dispatch/api/reference_targets.go`), and the issue page's tree sends
 * one address per node - every comment, message, ask and typed block - so a long conversation
 * can carry it past that cap. The other bound is the request line a proxy in front of Dispatch
 * admits, which a full batch of comment or message addresses passes
 * (`REFERENCE_TARGETS_MAX_QUERY_CHARS`). The reads they cost are what this pins: one request for
 * a page inside both, which is every ordinary issue, and a batch per bound above them, with
 * every address named in exactly one of them.
 */

/** `count` distinct block addresses, numbered so their sorted order is not their written one. */
function addresses(count: number): string[] {
  return Array.from({ length: count }, (_, index) => `dispatch://CORE-1/spec#b-r${index}`);
}

/** `count` distinct addresses of the length a conversation's own carry: a comment's id is a
 *  UUID, and the block inside it is another, so one address is about 90 characters encoded. */
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
    // nothing at all, which is what the slice this replaced did to everything past the first.
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

// A batch inside the server's cap can still be a request line no proxy in front of Dispatch
// admits: 247 addresses of a conversation's length encode to about 17 KB against a load
// balancer's 16 KB (`reference_targets.go:24-27`). Refused there, the read would draw every
// typed-block row of the heaviest issues blank, so the length is a bound of its own.
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
        `a batch of ${batch.length} addresses writes ${query.length} characters of query string`
      ).toBeLessThanOrEqual(REFERENCE_TARGETS_MAX_QUERY_CHARS);
    }
    // Nothing is lost to the second bound either: every address is asked for, once.
    expect(answer.targets.map((target) => target.ref)).toEqual([...refs].sort());
    expect(batches.flat().length).toBe(refs.length);
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
