import { expect, test } from "bun:test";

import { graphNode } from "../../__tests__/graph-fixture";
import type { Decision } from "../../api/types";
import { fromDecision } from "./adapters";
import { decision } from "./decision-fixture";

/** The bare `decisions` member, whose asks and links the read left out (`null`), of a decision
 *  its maker was unsure of. */
function model(fields: Partial<Decision> = {}): Decision {
  return decision("store", { asks: null, links: null, unsure: true, ...fields });
}

test("the bare member reads its unsure as the level and holds no author, level or reason", () => {
  const made = model();
  expect(fromDecision(made)).toEqual({
    asks: [],
    changed_at: made.changed_at,
    heading: made.heading,
    kept_by: undefined,
    kind: "agent",
    links: [],
    replaced: false,
    text: made.text,
    text_tree: undefined,
    unsure: true,
    waiting_review: true,
  });
});

test("a Keep names its person by login, and a person's decision is a person's", () => {
  const shown = fromDecision(
    model({
      asks: ["ask-1"],
      kept: {
        artifact_id: "doc",
        block_id: "store",
        changed_version: 3,
        kept_at: "2026-11-02T00:00:00Z",
        kept_by: "alice",
      },
      kind: "person",
    })
  );
  expect(shown.kept_by).toEqual({ id: "alice", kind: "user" });
  expect(shown.kind).toBe("person");
  expect(shown.asks).toEqual(["ask-1"]);
});

test("only a tie to a block is a statement the decision shaped, with the tie's own times", () => {
  const shown = fromDecision(
    model({
      links: [
        {
          created_at: "2026-11-01T00:00:00Z",
          created_version: 1,
          from_block: "store-why",
          stale: true,
          to: graphNode({
            block_id: "p1",
            changed_at: "2026-11-03T00:00:00Z",
            id: "CORE-1/spec#p1",
            kind: "block",
            text: "Notices are stored.",
          }),
        },
        {
          created_at: "2026-11-01T00:00:00Z",
          created_version: 1,
          from_block: "store-why",
          stale: false,
          to: graphNode({ id: "CORE-1/spec", kind: "document" }),
        },
      ],
    })
  );
  expect(shown.links).toEqual([
    {
      block_id: "p1",
      changed_at: "2026-11-03T00:00:00Z",
      created_at: "2026-11-01T00:00:00Z",
      stale: true,
      text: "Notices are stored.",
    },
  ]);
});
