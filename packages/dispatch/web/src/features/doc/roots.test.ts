import { expect, test } from "bun:test";
import * as Y from "yjs";

import type { Artifact } from "../../api/types";
import { fragmentOf } from "./roots";

/** An issue's plan, as the artifact read answers it before it names a fragment. */
const plan: Artifact = {
  created_at: "2026-11-01T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "0b4f6c1e-9d2a-4e7b-8c35-2f1a6d9e7b40",
  issue_key: "CORE-1",
  kind: "doc",
  name: "plan.md",
  primary: false,
  project: "CORE",
  ref_key: "CORE-1/plan-md",
  session_id: null,
  slug: "plan-md",
  versions: [],
};

/** An issue's room: its own fragment, `prosemirror`, and the plan's, named by the plan's id. */
function issueRoom(): Y.Doc {
  const room = new Y.Doc();
  room.getXmlFragment("prosemirror").insert(0, [new Y.XmlText("The issue's own node.")]);
  room.getXmlFragment(plan.id).insert(0, [new Y.XmlText("The plan's text.")]);
  return room;
}

test("the page binds the fragment the artifact read names", () => {
  const room = issueRoom();

  expect(room.getXmlFragment(fragmentOf({ ...plan, fragment: plan.id })).toString()).toBe(
    "The plan's text."
  );
});

test("the page binds prosemirror for an artifact read that names no fragment", () => {
  const room = issueRoom();

  expect(fragmentOf(plan)).toBe("prosemirror");
  expect(fragmentOf({ ...plan, fragment: null })).toBe("prosemirror");
  expect(room.getXmlFragment(fragmentOf(plan)).toString()).toBe("The issue's own node.");
});
