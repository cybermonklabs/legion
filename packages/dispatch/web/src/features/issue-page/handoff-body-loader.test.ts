import { describe, expect, spyOn, test } from "bun:test";
import { HANDOFF_BODIES_MAX_IDS } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";

import { jump, onFakeClock, settle, tick } from "../../__tests__/fake-clock";
import { api } from "../../api/client";
import type { HandoffBodies } from "../../api/types";
import { HandoffBodyLoader, handoffBodyKey } from "./handoff-body-loader";

// The bodies' loader of the issue's history (the plan's D13): told which handoff entries are in
// the band (within one screen of Details' view), that the reader scrolled, that an answer was
// drawn, that the history landed and that a Retry was pressed, it asks for the bodies by
// `GET /api/v1/issues/{key}/handoffs/bodies`, one request out at a time. Table B is its whole
// behaviour, one row each, on a clock the row moves.

const KEY = "ACME-1";

/** The ids of `count` handoffs from `h<from>` on, in the order the history draws their entries,
 *  newest first. */
function handoffIds(count: number, from = 1): string[] {
  return Array.from({ length: count }, (_, index) => `h${from + index}`);
}

/** The body a handoff of the rows is stored with. */
function bodyOf(id: string): string {
  return JSON.stringify({ summary: `The body of ${id}` });
}

/** One request the loader sent, held until the row answers it. */
interface Asked {
  readonly ids: readonly string[];
  readonly key: string;
  /** Answers each id asked with its body, but each of `left`, which the answer names as left,
   *  and each of `missing`, whose body is `null`, as for an id that is no handoff of the issue. */
  readonly answer: (left?: readonly string[], missing?: readonly string[]) => Promise<void>;
  /** Fails the request, as a refusal or a network failure does. */
  readonly fail: () => Promise<void>;
}

/** What a row is handed: the loader of `KEY`, each request it sent in the order sent, and the
 *  query client that holds the bodies. */
interface Loading {
  readonly asked: Asked[];
  readonly client: QueryClient;
  readonly loader: HandoffBodyLoader;
  /** The ids of request `index`, or undefined where the loader sent no such request. */
  readonly idsOf: (index: number) => readonly string[] | undefined;
  /** Whether the body of `id` is held for the page. */
  readonly held: (id: string) => boolean;
}

/** Runs `body` on the fake clock with a loader of `KEY`, each bodies read it sends held until the
 *  row answers it, and closes the loader and restores the read however the row ends. */
async function withLoader(body: (loading: Loading) => Promise<void>): Promise<void> {
  const asked: Asked[] = [];
  const read = spyOn(api, "getHandoffBodies").mockImplementation((key, ids) => {
    const { promise, reject, resolve } = Promise.withResolvers<HandoffBodies>();
    asked.push({
      answer: async (left = [], missing = []) => {
        resolve({
          bodies: ids
            .filter((id) => !left.includes(id))
            .map((id) => ({ body: missing.includes(id) ? null : bodyOf(id), id })),
          left: ids.filter((id) => left.includes(id)),
        });
        await settle();
      },
      fail: async () => {
        reject(new Error("the bodies are down"));
        await settle();
      },
      ids: [...ids],
      key,
    });
    return promise;
  });
  const client = new QueryClient();
  const loader = new HandoffBodyLoader(KEY, client);
  await onFakeClock(async () => {
    try {
      await body({
        asked,
        client,
        held: (id) => client.getQueryData(handoffBodyKey(id)) !== undefined,
        idsOf: (index) => asked[index]?.ids,
        loader,
      });
    } finally {
      loader.close();
      read.mockRestore();
      client.clear();
    }
  });
}

describe("the loader asks as its table says", () => {
  test("B1: it starts with 6 entries in the band: one request for the 6 ids, in the order drawn", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.landed();
      loader.band(handoffIds(6));
      expect(asked).toHaveLength(1);
      expect(asked[0]?.key).toBe(KEY);
      expect(idsOf(0)).toEqual(handoffIds(6));
      await asked[0]?.answer();
      await tick(3_000);
      expect(asked, "a request of every body answered was followed by another").toHaveLength(1);
    });
  });

  test("B2: it starts with 25 in the band, then the reader scrolls: 20 ids, and the other 5 at the scroll", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(25));
      loader.landed();
      expect(HANDOFF_BODIES_MAX_IDS).toBe(20);
      expect(idsOf(0)).toEqual(handoffIds(HANDOFF_BODIES_MAX_IDS));
      await asked[0]?.answer();
      expect(asked, "the entries past 20 were asked for with no scroll").toHaveLength(1);
      loader.scrolled();
      expect(idsOf(1)).toEqual(handoffIds(5, 21));
    });
  });

  test("B3: the same, no scroll: the other 5 two seconds after the first answer is drawn", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(25));
      loader.landed();
      await asked[0]?.answer();
      await tick(3_000);
      expect(asked, "the entries past 20 were asked for before the answer was drawn").toHaveLength(
        1
      );
      loader.drawn();
      await tick(1_900);
      expect(asked, "the entries past 20 were asked for sooner than 2 s").toHaveLength(1);
      await tick(200);
      expect(idsOf(1)).toEqual(handoffIds(5, 21));
    });
  });

  test("B4: an answer leaves 2 ids: they are sent at the next scroll, or 2 seconds after the answer is drawn", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(6));
      loader.landed();
      await asked[0]?.answer(["h5", "h6"]);
      expect(asked, "the ids left were asked for at once").toHaveLength(1);
      loader.scrolled();
      expect(idsOf(1)).toEqual(["h5", "h6"]);
      await asked[1]?.answer();
    });
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(6));
      loader.landed();
      await asked[0]?.answer(["h5", "h6"]);
      loader.drawn();
      await tick(1_900);
      expect(asked, "the ids left were asked for sooner than 2 s").toHaveLength(1);
      await tick(200);
      expect(idsOf(1)).toEqual(["h5", "h6"]);
    });
  });

  test("B5: a left id's entry goes out of the band before that: it is not sent, and it is sent at once when the entry comes back", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(6));
      loader.landed();
      await asked[0]?.answer(["h5", "h6"]);
      loader.band(handoffIds(5));
      loader.drawn();
      await tick(2_100);
      expect(idsOf(1), "an id whose entry left the band was asked for").toEqual(["h5"]);
      await asked[1]?.answer();
      loader.scrolled();
      await tick(3_000);
      expect(asked).toHaveLength(2);
      loader.band(handoffIds(6));
      expect(idsOf(2), "the entry come back into the band was not asked for at once").toEqual([
        "h6",
      ]);
    });
  });

  test("B6: a scroll brings a new entry into the band: its id is sent in that frame, one request for all that came in", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(2));
      loader.landed();
      await asked[0]?.answer();
      loader.scrolled();
      expect(asked, "a scroll with nothing left to ask sent a request").toHaveLength(1);
      loader.band(handoffIds(4));
      expect(idsOf(1)).toEqual(["h3", "h4"]);
      expect(asked).toHaveLength(2);
    });
  });

  test("B7: ids come in while a request is out: they are sent when the answer is in, in one request", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(2));
      loader.landed();
      loader.band(handoffIds(3));
      loader.band(handoffIds(4));
      loader.scrolled();
      expect(asked, "a second request went out while the first was").toHaveLength(1);
      await asked[0]?.answer();
      expect(idsOf(1)).toEqual(["h3", "h4"]);
      expect(asked).toHaveLength(2);
    });
  });

  test("B8: a request fails, then Retry on one entry: each of its ids is failed, and Retry sends one request for those still drawn", async () => {
    await withLoader(async ({ asked, held, idsOf, loader }) => {
      let told = 0;
      const stop = loader.subscribe(() => {
        told += 1;
      });
      loader.band(handoffIds(3));
      loader.landed();
      expect(loader.state("h2").reading).toBe(true);
      told = 0;
      await asked[0]?.fail();
      expect(told, "the entries were not told of the failure").toBeGreaterThan(0);
      for (const id of handoffIds(3)) {
        expect(loader.state(id), `the state of ${id}`).toEqual({ failures: 1, reading: false });
      }
      await tick(3_000);
      loader.scrolled();
      expect(asked, "a failed request was sent again with no Retry").toHaveLength(1);

      // The entry of h2 is no longer drawn in the band; Retry is pressed on h3's.
      loader.band(["h1", "h3"]);
      loader.retry("h3");
      expect(idsOf(1)).toEqual(["h1", "h3"]);
      expect(loader.state("h1").reading).toBe(true);
      expect(loader.state("h3")).toEqual({ failures: 1, reading: true });
      loader.retry("h1");
      loader.retry("h3");
      expect(asked, "a Retry pressed while its read is out sent another").toHaveLength(2);
      await asked[1]?.answer();
      expect(held("h1") && held("h3")).toBe(true);
      expect(loader.state("h3")).toEqual({ failures: 0, reading: false });
      expect(loader.state("h2"), "an entry no longer drawn was read again").toEqual({
        failures: 1,
        reading: false,
      });
      expect(held("h2")).toBe(false);
      stop();
    });
  });

  test("B9: an answer holds null for an id: that id is failed", async () => {
    await withLoader(async ({ asked, held, loader }) => {
      loader.band(handoffIds(3));
      loader.landed();
      await asked[0]?.answer([], ["h2"]);
      expect(loader.state("h2")).toEqual({ failures: 1, reading: false });
      expect(held("h2")).toBe(false);
      expect(held("h1") && held("h3")).toBe(true);
      expect(loader.state("h1")).toEqual({ failures: 0, reading: false });
    });
  });

  test("B10: an entry whose body is held is drawn again: nothing is sent", async () => {
    await withLoader(async ({ asked, client, held, loader }) => {
      loader.band(["h1"]);
      loader.landed();
      await asked[0]?.answer();
      loader.band([]);
      loader.band(["h1"]);
      loader.scrolled();
      loader.drawn();
      await tick(3_000);
      expect(asked, "a body held was asked for again").toHaveLength(1);
      // Details shut and opened again: a loader of its own, over the bodies the page holds.
      loader.close();
      const again = new HandoffBodyLoader(KEY, client);
      try {
        again.band(["h1"]);
        again.landed();
        expect(asked, "a body held was asked for again by the next loader").toHaveLength(1);
        // A body is held for the page's life, not for a read's default 5 minutes.
        await jump(600_000);
        expect(held("h1"), "the body held was let go").toBe(true);
      } finally {
        again.close();
      }
    });
  });

  test("B11: a folded handoff, then the reader opens it: nothing is sent while it is folded, and its id at once when opened", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      // h2 is folded, so its entry is drawn with no place in the band.
      loader.band(["h1"]);
      loader.landed();
      expect(idsOf(0)).toEqual(["h1"]);
      await asked[0]?.answer();
      loader.drawn();
      loader.scrolled();
      await tick(3_000);
      expect(asked, "the folded handoff's body was asked for").toHaveLength(1);
      loader.band(["h1", "h2"]);
      expect(idsOf(1)).toEqual(["h2"]);
    });
  });

  test("B12: an address names a handoff: the loader starts only after the landing, and that id is the first of the request", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(handoffIds(3));
      loader.drawn();
      loader.scrolled();
      await tick(3_000);
      expect(asked, "the loader asked before the history landed").toHaveLength(0);
      loader.landed("h3");
      expect(idsOf(0)).toEqual(["h3", "h1", "h2"]);
      expect(asked).toHaveLength(1);
    });
    // Landed before the band is first told: the request waits for the band and leads with the id.
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.landed("h9");
      expect(asked, "the loader asked before the band was told").toHaveLength(0);
      loader.band(["h1", "h9", "h2"]);
      expect(idsOf(0)).toEqual(["h9", "h1", "h2"]);
      expect(asked).toHaveLength(1);
    });
  });

  test("B13: Details shuts with a request out and a timer set: nothing more is sent, and the answer is kept when it comes", async () => {
    await withLoader(async ({ asked, held, idsOf, loader }) => {
      loader.band(handoffIds(25));
      loader.landed();
      await asked[0]?.answer();
      loader.drawn();
      // The timer for the 5 past 20 is set; a new entry's request goes out beside it.
      loader.band(handoffIds(26));
      expect(idsOf(1)).toEqual(["h26"]);
      loader.close();
      await tick(3_000);
      loader.scrolled();
      loader.band(handoffIds(27));
      loader.retry("h21");
      expect(asked, "a shut loader sent a request").toHaveLength(2);
      await asked[1]?.answer();
      expect(held("h26"), "the answer that came after Details shut was dropped").toBe(true);
      await tick(3_000);
      expect(asked).toHaveLength(2);
    });
  });

  test("B14: a handoff arrives live in the band: its id is sent at once", async () => {
    await withLoader(async ({ asked, idsOf, loader }) => {
      loader.band(["h1"]);
      loader.landed();
      await asked[0]?.answer();
      loader.drawn();
      // The new handoff is drawn at the head of the history, in the band.
      loader.band(["h2", "h1"]);
      expect(idsOf(1)).toEqual(["h2"]);
      expect(asked).toHaveLength(2);
    });
  });
});
