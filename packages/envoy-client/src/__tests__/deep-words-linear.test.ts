// A notice that names no session is drawn with its `words` object's blocks (`daemonFields`), and
// the fields beside them are read without the words (`withoutWords`). That walk copies the chain
// of objects above each level at every level, so its time and memory grow with the square of the
// payload's depth: a 60 KB payload 10,000 levels deep took 1.2 s and 987 MB of memory at
// ccf24d8a, where the same payload with no `words` object took 0.09 s and 229 MB; one of 200 KB
// (40,000 levels) took 3.6 s and 2.4 GB, and a second render in the same process was ended by
// the kernel (INFRA-3686 tester red test, round 23, class of PRRT_kwDORFy7ds67UiaE).
import { expect, test } from "bun:test";
import { renderInbound } from "../delivery";

const reader = "01a01111-2222-7333-4444-555555555555";
const DEPTH = 10_000;

function deepNotice(words: boolean): string {
  const head = words
    ? `{"words":{"summary":{"by":"implementer"}},"summary":"Done.","a":`
    : `{"summary":"Done.","a":`;
  return JSON.stringify({
    event_id: words ? "evt-deep-words" : "evt-deep-plain",
    source: "agent",
    source_event_id: "agent.daemon.deep",
    topic: "notifications.role.legion-acme-acme-1-architect",
    dedupe_key: "deep",
    issued_at: 1,
    payload_summary: "deep",
    trace_id: "trace-1",
    payload: `${head}${'{"a":'.repeat(DEPTH)}1${"}".repeat(DEPTH + 1)}`,
  });
}

function renderMs(frame: string): number {
  const start = performance.now();
  renderInbound(frame, reader);
  return performance.now() - start;
}

test("a daemon payload with a words object 10,000 deep renders in about the time of one without", () => {
  renderMs(deepNotice(false));
  const plain = renderMs(deepNotice(false));
  const words = renderMs(deepNotice(true));
  expect(words).toBeLessThan(3 * plain + 250);
}, 60_000);
