import { describe, expect, test } from "bun:test";
import { labelsOf, noTextDetail, servedModelFault } from "../models";

/**
 * The reading of an answer, over hand-written bodies: nothing here calls an endpoint or reads a
 * key. What it holds the parser to is the rule the round scores on - a line that is a label, or
 * one label qualified - the shape of the detail an answer with no text leaves behind, and which
 * served model an answer may name for its verdict to be read as the asked model's.
 */

/** The labels of the measured question, whose answer qualified the one it chose. */
const LABELS: readonly string[] = ["Dismiss the review on #3170", "Do not dismiss"];

/** The measured answer: the label, a colon, and why. */
const QUALIFIED = "Dismiss the review on #3170: one click, the merge goes and ctx-memory unfreezes";

describe("labelsOf", () => {
  test("takes a line that is a label whole", () => {
    expect(labelsOf("Do not dismiss", LABELS)).toEqual(["Do not dismiss"]);
  });

  test("takes a label whole through the punctuation a model puts around it", () => {
    expect(labelsOf('  - "do not dismiss."  ', LABELS)).toEqual(["Do not dismiss"]);
    expect(labelsOf("Here is my answer\n\n* Do not dismiss", LABELS)).toEqual(["Do not dismiss"]);
  });

  test("takes every label of an answer that names several whole, which a multiple ask asks for", () => {
    expect(labelsOf("Dismiss the review on #3170\nDo not dismiss", LABELS)).toEqual([
      "Dismiss the review on #3170",
      "Do not dismiss",
    ]);
  });

  test("takes the label of the measured answer, which qualified it after a colon", () => {
    expect(labelsOf(QUALIFIED, LABELS)).toEqual(["Dismiss the review on #3170"]);
  });

  test("takes a label qualified after each separator", () => {
    const after = [
      ": one click",
      " - one click",
      " \u2013 one click",
      "\u2014 one click",
      ", since the merge goes",
      "; the merge goes",
      ". The merge goes",
      " (the merge goes)",
      "[1]",
    ];
    for (const tail of after) {
      expect([tail, labelsOf(`Do not dismiss${tail}`, LABELS)]).toEqual([tail, ["Do not dismiss"]]);
    }
  });

  test("takes a qualified label from the line it is at the head of", () => {
    const text = `I weighed both options.\n${QUALIFIED}\nThat is my answer.`;
    expect(labelsOf(text, LABELS)).toEqual(["Dismiss the review on #3170"]);
  });

  test("reads nothing from an answer naming two labels, which chose nothing", () => {
    const text = `Dismiss the review on #3170: the merge goes.\nDo not dismiss: it is unread.`;
    expect(labelsOf(text, LABELS)).toEqual([]);
  });

  test("reads nothing from an answer naming no label", () => {
    expect(labelsOf("I cannot tell from what I was shown.", LABELS)).toEqual([]);
  });

  test("reads nothing from a label named inside a line rather than at its head", () => {
    expect(labelsOf("I would dismiss the review on #3170 today.", LABELS)).toEqual([]);
  });

  test("takes the longer of two labels where one is a prefix of the other", () => {
    const prefixed = ["Dismiss", "Dismiss the review on #3170"];
    expect(labelsOf(QUALIFIED, prefixed)).toEqual(["Dismiss the review on #3170"]);
    expect(labelsOf("Dismiss: one click and the merge goes", prefixed)).toEqual(["Dismiss"]);
  });

  test("reads nothing where a prefix label is separated from the rest of the longer one", () => {
    const prefixed = ["Approve", "Approve (version 3)"];
    expect(labelsOf("Approve (version 3): it is ready", prefixed)).toEqual([]);
  });

  test("takes the whole label where a shorter label of the case is its first word", () => {
    // The labels of a case of this corpus, where `Done` is the first word of the longer label
    // and the longer label ends in a full stop: 23 of the corpus's cases offer such a pair.
    const measured = [
      "Done",
      "Done, but with a different name or size (say which in a comment).",
      "Can't",
    ];
    expect(
      labelsOf("Done, but with a different name or size (say which in a comment).", measured)
    ).toEqual(["Done, but with a different name or size (say which in a comment)."]);
    expect(labelsOf("Done: it is merged", measured)).toEqual(["Done"]);
  });
});

describe("noTextDetail", () => {
  test("names the stop reason and the blocks an answer with no text held", () => {
    const body = {
      stop_reason: "max_tokens",
      content: [{ type: "thinking", thinking: "", signature: "CAQS" }],
    };
    expect(noTextDetail(body)).toBe("max_tokens, blocks thinking");
  });

  test("names each block kind once, in the order they came", () => {
    const body = {
      stop_reason: "end_turn",
      content: [{ type: "thinking" }, { type: "thinking" }, { type: "tool_use" }],
    };
    expect(noTextDetail(body)).toBe("end_turn, blocks thinking, tool_use");
  });

  test("says so where the answer held no block and named no stop reason", () => {
    expect(noTextDetail({ content: [] })).toBe("no stop reason, no blocks");
  });
});

describe("servedModelFault", () => {
  const ASKED = "anthropic/claude-opus-5-5";

  test("takes the asked id, as the gateway echoes it and the agent reports it", () => {
    expect(servedModelFault(ASKED, ASKED, true)).toBeNull();
    expect(servedModelFault("claude-opus-5-5", "claude-opus-5-5", true)).toBeNull();
  });

  test("refuses the models a fallback ladder steps down to, naming the one that answered", () => {
    expect(servedModelFault(ASKED, "anthropic/claude-fable-5-2", true)).toBe(
      `asked ${ASKED}, and the answer came from anthropic/claude-fable-5-2`
    );
    expect(servedModelFault(ASKED, "anthropic/claude-opus-5", true)).not.toBeNull();
  });

  test("refuses a dated snapshot, another provider's route, or the bare name of the asked id", () => {
    for (const served of [
      "anthropic/claude-opus-5-5-20261015",
      "pbc/claude-opus-5-5",
      "claude-opus-5-5",
    ]) {
      expect(servedModelFault(ASKED, served, true)).toBe(
        `asked ${ASKED}, and the answer came from ${served}`
      );
    }
  });

  test("refuses every served id for an ask that names no model", () => {
    expect(servedModelFault("", ASKED, true)).not.toBeNull();
  });

  test("holds a Jev answer, asked by an alias, to naming a model and not to the alias", () => {
    expect(servedModelFault("jev-latest", "jev-2026-11", false)).toBeNull();
    expect(servedModelFault("jev-latest", null, false)).toBe(
      "asked jev-latest, and the answer names no model"
    );
  });

  test("refuses an answer that names no model on every endpoint, an empty name included", () => {
    for (const exactId of [true, false]) {
      expect(servedModelFault(ASKED, null, exactId)).toBe(
        `asked ${ASKED}, and the answer names no model`
      );
      expect(servedModelFault(ASKED, "", exactId)).toBe(
        `asked ${ASKED}, and the answer names no model`
      );
    }
  });
});
