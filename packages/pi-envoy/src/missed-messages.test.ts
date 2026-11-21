import { expect, test } from "bun:test";
import { INBOUND_OUTAGE_ENTRY, inboundOutageTold } from "./missed-messages";

/** The transcript entry `pi.appendEntry(INBOUND_OUTAGE_ENTRY, data)` writes. */
function outageEntry(data: unknown): Record<string, unknown> {
  return { type: "custom", customType: INBOUND_OUTAGE_ENTRY, data };
}

test("the newest outage record is what the session was last told, and one that does not parse reads as not told, never as an older told one", () => {
  const told = outageEntry({ told: true });
  expect(inboundOutageTold([])).toBe(false);
  expect(inboundOutageTold([told])).toBe(true);
  expect(inboundOutageTold([told, outageEntry({ told: false })])).toBe(false);
  // The session's last record, its `{told: false}` damaged in the transcript: read in place of it,
  // the older told record would tell the session the end of an outage it was told had ended.
  expect(inboundOutageTold([told, outageEntry({ told: "false" })])).toBe(false);
  expect(inboundOutageTold([told, outageEntry(null)])).toBe(false);
});
