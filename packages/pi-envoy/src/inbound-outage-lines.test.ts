import { expect, test } from "bun:test";
import { INBOUND_RESTORED_LINES, INBOUND_UNAVAILABLE_LINES } from "./inbound-outage-lines";

// Each kind of outage is logged and noted in the pane in words of its own, and a refusal and a
// lost relay subject in the same words: the session receives nothing, then receives again. A
// kind the table lacks fails here.
test("each kind of outage is logged and noted in the pane in exactly its own words", () => {
  const unavailable = Object.fromEntries(
    Object.entries(INBOUND_UNAVAILABLE_LINES).map(([kind, lines]) => [
      kind,
      { log: lines.log("WHY"), pane: lines.pane("WHY") },
    ])
  );

  expect(unavailable).toEqual({
    unrecorded: {
      log: "envoy: this session's registration is not recorded for certain: WHY",
      pane: "envoy: this session's registration is not recorded for certain (WHY), and the session registers again at every heartbeat",
    },
    github_withheld: {
      log: "envoy: the listener follows none of this session's GitHub topics now; the session's other messages still reach it if the listener already held its registration, and none reach it until a registration is accepted if it held none: WHY",
      pane: "envoy: GitHub messages are withheld (WHY); other messages still arrive if the listener already held this session's registration, and none arrive until a registration is accepted if it held none; registering again every heartbeat",
    },
    refused: {
      log: "envoy: this session receives nothing: WHY",
      pane: "envoy: inbound messages are unavailable (WHY); registering again every heartbeat",
    },
    no_relay: {
      log: "envoy: this session receives nothing: WHY",
      pane: "envoy: inbound messages are unavailable (WHY); registering again every heartbeat",
    },
  });
});

test("the end of each kind of outage is logged and noted in the pane in exactly its own words", () => {
  expect(INBOUND_RESTORED_LINES).toEqual({
    unrecorded: {
      log: "envoy: the listener recorded a registration of this session",
      pane: "envoy: the listener recorded a registration of this session",
    },
    github_withheld: {
      log: "envoy: the listener follows this session's GitHub topics again: a registration was accepted",
      pane: "envoy: the listener follows this session's GitHub topics again",
    },
    refused: {
      log: "envoy: this session receives again: a registration was accepted",
      pane: "envoy: inbound messages are back",
    },
    no_relay: {
      log: "envoy: this session receives again: a registration was accepted",
      pane: "envoy: inbound messages are back",
    },
  });
});
