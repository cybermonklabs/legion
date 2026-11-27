import { expect, test } from "bun:test"
import { INBOUND_RESTORED_LINES, INBOUND_UNAVAILABLE_LINES } from "../src/inbound-outage-lines"

// Each kind of outage is written to stderr in words of its own, and a refusal and a lost relay
// subject in the same words: the session receives nothing, then receives again. A kind the table
// lacks fails here.
test("each kind of outage is written to stderr in exactly its own words", () => {
  const unavailable = Object.fromEntries(
    Object.entries(INBOUND_UNAVAILABLE_LINES).map(([kind, lines]) => [
      kind,
      { line: lines.line("WHY"), told: lines.told },
    ]),
  )

  expect(unavailable).toEqual({
    unrecorded: {
      line: "envoy-channel: this session's registration is not recorded for certain (WHY), and the session registers again at every heartbeat\n",
      told: "that its registration is not recorded for certain",
    },
    github_withheld: {
      line: "envoy-channel: the listener follows none of this session's GitHub topics now (WHY); the session's other messages still reach it if the listener already held its registration, and none reach it until a registration is accepted if it held none; registering again every heartbeat\n",
      told: "that its GitHub messages are withheld",
    },
    refused: {
      line: "envoy-channel: this session receives nothing (WHY); registering again every heartbeat\n",
      told: "that it receives nothing",
    },
    no_relay: {
      line: "envoy-channel: this session receives nothing (WHY); registering again every heartbeat\n",
      told: "that it receives nothing",
    },
  })
})

test("the end of each kind of outage is written to stderr in exactly its own words", () => {
  expect(INBOUND_RESTORED_LINES).toEqual({
    unrecorded: {
      line: "envoy-channel: the listener recorded a registration of this session\n",
      told: "that the listener recorded its registration",
    },
    github_withheld: {
      line: "envoy-channel: the listener follows this session's GitHub topics again: a registration was accepted\n",
      told: "that the listener follows its GitHub topics again",
    },
    refused: {
      line: "envoy-channel: this session receives again: a registration was accepted\n",
      told: "that it receives again",
    },
    no_relay: {
      line: "envoy-channel: this session receives again: a registration was accepted\n",
      told: "that it receives again",
    },
  })
})
