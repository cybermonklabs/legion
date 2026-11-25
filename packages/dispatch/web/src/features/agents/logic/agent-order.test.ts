import { expect, test } from "bun:test";

import type { Agent } from "../../../api/types";
import { orderAgents } from "./agent-order";

const NOW = 1_762_000_000_000;

/** A live session with no Dispatch signal unless the overrides give it one. */
function session(overrides: Partial<Agent> & Pick<Agent, "session_id">): Agent {
  return {
    capabilities: [],
    dir: "/src/legion",
    host: "devbox",
    last_activity: null,
    last_seen: NOW - 30_000,
    machine_id: "listener",
    open_asks: 0,
    roles: [],
    // The server always writes a session's state (model.Agent), so the document requires it.
    state: "live",
    title: overrides.session_id,
    ...overrides,
  };
}

function minutesAgo(minutes: number): string {
  return new Date(NOW - minutes * 60_000).toISOString();
}

test("orderAgents puts who needs you first, then open asks, then Dispatch recency with silent sessions last", () => {
  const needsYou = session({ last_activity: minutesAgo(30), open_asks: 1, session_id: "needy" });
  const owed = session({ last_activity: minutesAgo(20), open_asks: 3, session_id: "owed" });
  const recent = session({ last_activity: minutesAgo(1), session_id: "recent" });
  const older = session({ last_activity: minutesAgo(5), session_id: "older" });
  const silentB = session({ session_id: "silent-b", title: "Beta" });
  const silentA = session({ session_id: "silent-a", title: "Alpha" });
  const ordered = orderAgents(
    [silentB, older, recent, owed, silentA, needsYou],
    [],
    new Map([["needy", 1]])
  );
  expect(ordered.map((agent) => agent.session_id)).toEqual([
    "needy",
    "owed",
    "recent",
    "older",
    "silent-a",
    "silent-b",
  ]);
});

test("orderAgents keeps pinned sessions first in pin order, whoever needs you", () => {
  const needsYou = session({ last_activity: minutesAgo(1), open_asks: 1, session_id: "needy" });
  const pinnedSilent = session({ session_id: "pinned-silent" });
  const pinnedRecent = session({ last_activity: minutesAgo(2), session_id: "pinned-recent" });
  const ordered = orderAgents(
    [needsYou, pinnedRecent, pinnedSilent],
    ["pinned-silent", "pinned-recent"],
    new Map([["needy", 1]])
  );
  expect(ordered.map((agent) => agent.session_id)).toEqual([
    "pinned-silent",
    "pinned-recent",
    "needy",
  ]);
});

test("orderAgents orders Dispatch activity within one second by time, not by the timestamp strings", () => {
  // As text `…00.12Z` sorts after `…00.123456Z`, the later time.
  const earlier = session({ last_activity: "2026-10-02T00:00:00.12Z", session_id: "earlier" });
  const later = session({ last_activity: "2026-10-02T00:00:00.123456Z", session_id: "later" });
  expect(orderAgents([earlier, later], [], new Map()).map((agent) => agent.session_id)).toEqual([
    "later",
    "earlier",
  ]);
});
