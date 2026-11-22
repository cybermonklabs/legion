import { expect, test } from "bun:test";

import { blockParts } from "./change-request";

// A node of a change request is read from two texts: the issue's block, whose attributes of The
// Spec are named as the issue writes them (`under`, `supports`, `statement`) beside its own
// placement in the conversation (`parent`, `serves`), and the approved version as The Spec's files
// print it (`parent`, `serves` for those same relations). Both read into the one set of names, so
// a person compares like with like, and neither shows the request's own mechanics (`base`,
// `removes`) or the issue's placement as a fact of the node.

test("an issue's block reads its id, its attributes of The Spec in its own names, and its body", () => {
  expect(
    blockParts(
      ':::requirement{#an-order-is-paid-once kind="behavior" under="billing-works" supports="a-bill" about="billing &amp; bank" serves="b-k1" parent="b-ask" base="1"}\nAn order is paid once, and a second payment is refused.\n:::',
      "issue"
    )
  ).toEqual({
    attributes: [
      ["kind", "behavior"],
      ["under", "billing-works"],
      ["supports", "a-bill"],
      ["about", "billing & bank"],
    ],
    body: "An order is paid once, and a second payment is refused.",
    id: "an-order-is-paid-once",
    type: "requirement",
  });
});

test("an approved version reads its relations in the issue's names", () => {
  expect(
    blockParts(
      ':::requirement{#an-order-is-paid-once kind="behavior" parent="billing-works" serves="a-bill" about="billing"}\nAn order is paid once.\n:::\n',
      "approved"
    )
  ).toEqual({
    attributes: [
      ["kind", "behavior"],
      ["under", "billing-works"],
      ["supports", "a-bill"],
      ["about", "billing"],
    ],
    body: "An order is paid once.",
    id: "an-order-is-paid-once",
    type: "requirement",
  });
  expect(
    blockParts(
      ':::check{#a-check serves="an-order-is-paid-once" subject="code" procedure="program" time_limit="24h0m0s"}\nClaim: A second payment is refused.\nProcedure: The test pays twice.\n:::\n',
      "approved"
    )
  ).toEqual({
    attributes: [
      ["statement", "an-order-is-paid-once"],
      ["subject", "code"],
      ["procedure", "program"],
      ["time_limit", "24h0m0s"],
    ],
    body: "Claim: A second payment is refused.\nProcedure: The test pays twice.",
    id: "a-check",
    type: "check",
  });
});

test("a check of an issue keeps each field of its body on a line of its own", () => {
  expect(
    blockParts(
      ':::check{#a-check statement="an-order-is-paid-once" subject="code" surface="cli" run="go test" base="1"}\nClaim: A second payment is refused.\n\nProcedure: The test pays twice.\n:::',
      "issue"
    )
  ).toEqual({
    attributes: [
      ["statement", "an-order-is-paid-once"],
      ["subject", "code"],
    ],
    body: "Claim: A second payment is refused.\n\nProcedure: The test pays twice.",
    id: "a-check",
    type: "check",
  });
});

test("a component and an interface read their name, their kind and what they join", () => {
  expect(
    blockParts(
      ':::component{#billing-to-bank name="Billing to bank" kind="interface" joins="bank billing" removes="true" base="1"}\nBilling sends each payment to the bank.\n:::',
      "issue"
    )
  ).toEqual({
    attributes: [
      ["name", "Billing to bank"],
      ["kind", "interface"],
      ["joins", "bank billing"],
    ],
    body: "Billing sends each payment to the bank.",
    id: "billing-to-bank",
    type: "component",
  });
});

test("a text that opens no block reads as its whole text, with no id and no attribute", () => {
  expect(blockParts("No block here.", "issue")).toEqual({
    attributes: [],
    body: "No block here.",
    id: "",
    type: "",
  });
});
