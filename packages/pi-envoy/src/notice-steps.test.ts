import { expect, test } from "bun:test";
import { sessionIdChangedText, unreadMessagesText } from "./envoy-notices";

// R2 of dispatch://INFRA-3686: "Each daemon text, and each plugin notice that the Design names, is
// built from a record of its kind ... No field holds a sentence that is the same in every text of
// its kind." The Design names Envoy's gap, inbound-unavailable and message-not-shown notices,
// pi-envoy's session-id notice and the wake for cards that came with no turn. Each is rendered
// twice from different values in every field; the lines both renderings hold, other than the root
// and the kind (`notice: <kind>`), are a field holding the same words in every notice of the kind.
test("no field of Envoy's own notices holds the same sentence in every notice of its kind", () => {
  const pairs: Record<string, readonly [string, string]> = {
    "session id changed": [
      sessionIdChangedText(
        "01a0aaaa-0000-7000-8000-000000000001",
        "01a0bbbb-0000-7000-8000-000000000002"
      ),
      sessionIdChangedText(
        "01a0cccc-0000-7000-8000-000000000003",
        "01a0dddd-0000-7000-8000-000000000004"
      ),
    ],
    "unread messages": [unreadMessagesText(3), unreadMessagesText(11)],
  };
  const shared: string[] = [];
  for (const [kind, [a, b]] of Object.entries(pairs)) {
    const second = new Set(b.split("\n"));
    for (const line of a.split("\n")) {
      if (line === "envoy:" || line.trimStart().startsWith("notice: ")) continue;
      if (second.has(line)) shared.push(`${kind}: ${line.trim()}`);
    }
  }
  expect(shared).toEqual([]);
});
