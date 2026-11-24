import { expect } from "bun:test";
import { REMOTE_TOKEN } from "@legion/test-fixtures/print-test";

// Whatever answers a script's gh call in GitHub's place holds gh's token, and gh hands that party's
// words on: on its stderr (gh prints the answer's message, or the client's error) and as its
// answer. So a script prints no word of either, beside a closed value it expects. A row puts MARKER
// where those words go, through a stand-in gh first on PATH, and holds all the script printed to
// markerRuns: a run of eight of the marker's characters is the piece the redaction rule never
// prints (packages/envoy/internal/redact/redacttest, Piece).
export const MARKER = REMOTE_TOKEN;

/** Each run of eight characters of MARKER that `text` holds; empty when it holds none. */
export function markerRuns(text: string): string[] {
  const runs: string[] = [];
  for (let at = 0; at + 8 <= MARKER.length; at++) {
    const run = MARKER.slice(at, at + 8);
    if (text.includes(run)) runs.push(run);
  }
  return runs;
}

/** Fails when `printed` holds a run of MARKER, quoting all it printed so a failing log shows it. */
export function expectNoMarker(printed: string): void {
  expect(markerRuns(printed), `what the script printed:\n${printed}`).toEqual([]);
}
