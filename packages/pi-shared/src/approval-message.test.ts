import { expect, test } from "bun:test";
import {
  APPROVAL_MESSAGE_TYPE,
  isApprovalMessage,
  splitTrailingApproval,
} from "./approval-message";

// Message shapes as Oh My Pi hands them to a `context` handler.
const ask = { role: "user", content: [{ type: "text", text: "Fix the bug." }], timestamp: 1 };
const reply = {
  role: "assistant",
  content: [{ type: "text", text: "Fixed." }],
  stopReason: "stop",
  timestamp: 4,
};

// The approval message as pi-legion places it, and an Envoy card, another plugin's custom message.
const approval = {
  role: "custom",
  customType: APPROVAL_MESSAGE_TYPE,
  content: [
    {
      type: "text",
      text: "A person has approved version 24 of the spec of ACME-2694 (dispatch://ACME-2694/spec@v24).",
    },
  ],
  display: false,
  attribution: "agent",
  timestamp: 20,
};
const laterApproval = { ...approval, timestamp: 21 };
const card = {
  role: "custom",
  customType: "envoy-message",
  content: "envoy:\n  notice: CI finished",
  display: true,
  timestamp: 5,
};

test("the approval message is a custom message of its own type", () => {
  expect(isApprovalMessage(approval)).toBe(true);
  expect(isApprovalMessage(card)).toBe(false);
  expect(isApprovalMessage({ ...ask, customType: APPROVAL_MESSAGE_TYPE })).toBe(false);
  expect(isApprovalMessage(undefined)).toBe(false);
});

test("a request is cut before its trailing run of approval messages, and one that ends with none is all head", () => {
  // A copy rebuilt right before its answer is no part of the trailing run.
  const messages = [ask, approval, reply, card, approval, laterApproval];
  expect(splitTrailingApproval(messages)).toEqual({
    head: [ask, approval, reply, card],
    tail: [approval, laterApproval],
  });
  const ending = [ask, approval, reply];
  const { head, tail } = splitTrailingApproval(ending);
  expect(head).toBe(ending);
  expect(tail).toEqual([]);
  expect(splitTrailingApproval([approval])).toEqual({ head: [], tail: [approval] });
});
