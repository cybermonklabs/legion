/** What the stubbed layer answers on `/ws/live`.
 *
 *  A HAR records no websocket and `page.routeWebSocket` has no replay mode, so a socket's frames
 *  are written by hand whatever this layer does. Writing them from
 *  `packages/contracts/fixtures/live/*.json` puts them behind a guard that already exists:
 *  `packages/envoy/internal/dispatch/api/delivery_live_diff_test.go` diffs every one of those
 *  files against what the Go server sends, so a frame shape that changes there fails Go's own
 *  tests rather than silently leaving this layer answering a shape the server no longer speaks.
 *
 *  What it answers, and nothing beyond it: a `subscribe` gets the fixture of its view's kind,
 *  carrying the subscription id the page asked under; a `heartbeat` gets a heartbeat. An
 *  `unsubscribe` is taken and answered with nothing, as the server answers it.
 *
 *  A row that needs a frame these fixtures do not hold - a delivery arriving while the page
 *  watches, an agent's turn streaming in - is a row that does not move to this layer. Its claim
 *  is the server's own behaviour, which is R2's exclusion.
 */
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const fixtures = fileURLToPath(new URL("../../../contracts/fixtures/live/", import.meta.url));

// One read per fixture per process: these files do not change while a run is going, and
// `answerTo` runs inside the socket's message handler, which a 9 KB read and parse per frame
// would block. The cached object is never handed out directly - `answerTo` spreads it - so no
// caller can mutate what the next frame is built from.
const loaded = new Map<string, Record<string, unknown>>();

function frame(name: string): Record<string, unknown> {
  let held = loaded.get(name);
  if (held === undefined) {
    held = JSON.parse(readFileSync(`${fixtures}${name}.json`, "utf8")) as Record<string, unknown>;
    loaded.set(name, held);
  }
  return held;
}

/** The first frame the server sends for a subscription of each view kind, by that kind. A kind
 *  absent here is one this layer does not answer, and a page that asks for it gets silence - the
 *  same thing it gets from a server that has not answered yet. */
const openingFrames: Record<string, string> = {
  agent: "agent",
  events: "events_start",
  timeline: "snapshot",
};

export interface LiveClientFrame {
  type?: string;
  sub?: string;
  view?: { kind?: string };
}

/** What to send in answer to one frame the page sent, or undefined where the server would send
 *  nothing. It needs no browser and no server: it reads the committed fixtures and answers
 *  from them, so a frame shape is decided here and not by whatever is listening. */
export function answerTo(sent: LiveClientFrame): Record<string, unknown> | undefined {
  if (sent.type === "heartbeat") return frame("heartbeat");
  if (sent.type !== "subscribe") return undefined;
  const opening = openingFrames[sent.view?.kind ?? ""];
  if (opening === undefined) return undefined;
  return { ...frame(opening), sub: sent.sub };
}
