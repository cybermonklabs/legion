import { expect, test } from "bun:test";
import { cardLines } from "./card-entries";

/** A card line as the host writes one into the session file. */
const card = (label: string): string =>
  JSON.stringify({
    type: "custom_message",
    customType: "envoy-message",
    details: { handOver: "idle", summary: label },
  });

const other = JSON.stringify({ type: "assistant", content: "not a card" });

test("the cards of a file are its envoy-message lines, in the order it holds them", () => {
  const text = `${card("a")}\n${other}\n${card("b")}\n`;
  expect(cardLines(text, "alive")).toEqual([card("a"), card("b")]);
});

// The control for this finding: the last line is a card the host has not finished writing, cut
// after both markers, which is the read that threw in packages/pi-legion's stall-watchdog case
// and would throw here too (dispatch://INFRA-2188). Reverting `writtenLines` to a plain
// `split("\n")` turns both assertions below red: the first gets a third entry, and the second
// throws instead of parsing.
test("a card line the host has not finished writing is withheld while the pane runs", () => {
  const torn = `${card("a")}\n${card("b")}\n${card("c").slice(0, 60)}`;
  expect(torn).toContain('"envoy-message"');
  const lines = cardLines(torn, "alive");
  expect(lines).toEqual([card("a"), card("b")]);
  expect(lines.map((line) => JSON.parse(line))).toHaveLength(2);
});

test("the same fragment is selected once the pane has exited, so parsing it throws", () => {
  const torn = `${card("a")}\n${card("c").slice(0, 60)}`;
  const lines = cardLines(torn, "gone");
  expect(lines).toHaveLength(2);
  expect(() => lines.map((line) => JSON.parse(line))).toThrow(/JSON Parse error/);
});
