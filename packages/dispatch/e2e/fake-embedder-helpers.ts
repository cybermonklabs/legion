import { test } from "@playwright/test";

import type { FakeEmbedCall, FakeEmbedCalls } from "./fake-embedder";
import { usesFakeEmbedder } from "./harness-embedder";
import { harnessPorts } from "./harness-ports";

function fixtureURL(path: string): string {
  return `http://127.0.0.1:${harnessPorts().fakeEmbedder.port}${path}`;
}

async function fixtureWrite(path: string, body: unknown): Promise<void> {
  const response = await fetch(fixtureURL(path), {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`fake embedder ${path}: ${response.status}`);
  }
}

/** Replaces the marker groups the fake embedder reads wholesale. Every text holding a word of
 *  one group is embedded as that group, so a query and the item a row means it to find take one
 *  vector and nothing else comes near either (`e2e/fake-embedder.ts`). The row that seeds them
 *  is skipped where this run's server embeds nowhere (`e2e/harness-embedder.ts`). */
export async function setEmbedMarkers(markers: readonly (readonly string[])[]): Promise<void> {
  test.skip(
    !usesFakeEmbedder,
    "this run's server embeds at no fake embedder (DISPATCH_E2E_EMBEDDER=fake)"
  );
  await fixtureWrite("/__fixture/markers", markers);
}

/** Forgets the calls counted so far, keeping the markers a row seeded, so a row that made the
 *  server embed while it was setting its fixture up counts from zero. */
export async function forgetEmbedCalls(): Promise<void> {
  await fixtureWrite("/__fixture/calls", {});
}

/** Every call the server has made to the fake embedder since the last reset, oldest first. */
export async function embedCalls(): Promise<readonly FakeEmbedCall[]> {
  const response = await fetch(fixtureURL("/__fixture/calls"));
  if (!response.ok) {
    throw new Error(`fake embedder /__fixture/calls: ${response.status}`);
  }
  return ((await response.json()) as FakeEmbedCalls).calls;
}

/** How many queries the server has embedded since the last reset: one per search that asked for
 *  meaning as well as words, and none at all behind a keystroke, which asks for the keyword leg
 *  alone. The texts are answered with the count so a failure says which queries were embedded,
 *  not only how many. */
export async function embeddedQueries(): Promise<readonly string[]> {
  return (await embedCalls())
    .filter((call) => call.input_type === "search_query")
    .flatMap((call) => [...call.texts]);
}

/** Clears the fake embedder between rows, so the calls one row made are never counted by the
 *  next and the markers it seeded reach none of them. A run whose server embeds nowhere started
 *  no fake embedder, and has nothing to clear. */
export async function resetFakeEmbedder(): Promise<void> {
  if (!usesFakeEmbedder) return;
  await fixtureWrite("/__fixture/reset", {});
}
