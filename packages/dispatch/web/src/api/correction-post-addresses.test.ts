import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { markCorrectedPost } from "./corrected-post";
import { eventRefreshTargets } from "./event-refresh";
import { referenceTargetsQuery } from "./queries";
import { refreshQueries } from "./query-refresh";
import type { DispatchEvent, ReferenceTarget } from "./types";

// R2 and R3 of dispatch://INFRA-1523, and the architect's decision of 2026-11-19 22:21Z. GET
// /api/v1/reference-targets names a comment or a message by the id its address writes, whatever
// owner the address is written under (`refs.TargetNames`, by `text.TargetID`, the id alone), and
// resolves an id written short among the items of the owner the address names alone
// (`refs.PrefixScope`, `refs.ResolvePrefixes`). A post's correction refreshes every names read
// whose key names the post so, and writes its own marks into the answer each read keeps, so a
// refetch that fails leaves no deleted post's words and no edited one's without its mark.

// Hex letters in both, so an id spelled in upper case is another spelling of it.
const POST = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d";
const OTHER = "a1b2c3d4-9999-4999-8999-999999999999";
const ALICE = { id: "alice", kind: "user" } as const;

/** A correction of the post `POST` on CORE-1, with `payload` beside its id. */
function correction(type: DispatchEvent["type"], payload: Record<string, unknown> = {}) {
  return {
    actor: ALICE,
    artifact_id: null,
    created_at: "2026-11-19T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: true,
    payload: { id: POST, issue_key: "CORE-1", ...payload },
    project: "CORE",
    seq: 1,
    type,
  } as unknown as DispatchEvent;
}

/** The address sets among `held` whose names read `event` refreshes: each set is cached under the
 *  key the page reads it by, and the event's rules run over the cache as the feed runs them. */
function refreshedNames(event: DispatchEvent, held: readonly string[][]): string[][] {
  const client = new QueryClient();
  for (const refs of held) {
    client.setQueryData(referenceTargetsQuery(refs).queryKey, { targets: [] });
  }
  refreshQueries(client, eventRefreshTargets(event, "bob"));
  return held.filter(
    (refs) => client.getQueryState(referenceTargetsQuery(refs).queryKey)?.isInvalidated === true
  );
}

// Every form the server reads as the post: its whole id under another issue, a project
// document's form with or without a version, and a block of the post, pinned or not. A message is
// an issue's alone, so it has no project document's form.
test("a correction refreshes a names read naming the post by its whole id in any form, under any owner", () => {
  const forms = {
    comment: [
      [`dispatch://CORE-2/comment/${POST}#b-b1`],
      [`dispatch://CORE-2/comment/${POST}#b-b1@v2`],
      [`dispatch://CORE-12/comment/${POST}`],
      [`dispatch://ACME/artifact/roadmap-md/comment/${POST}`],
      [`dispatch://ACME/artifact/roadmap-md@v2/comment/${POST}`],
      [`dispatch://ACME/artifact/roadmap-md/comment/${POST}#b-b1`],
    ],
    message: [[`dispatch://CORE-2/message/${POST}#b-b1`], [`dispatch://CORE-12/message/${POST}`]],
  };
  for (const type of [
    "comment.edited",
    "comment.deleted",
    "message.edited",
    "message.deleted",
  ] as const) {
    const naming = type.startsWith("comment") ? forms.comment : forms.message;
    expect(refreshedNames(correction(type), naming), type).toEqual(naming);
  }
});

// What the server does not read as the post: the same id written short under another owner,
// which it resolves among that owner's items; the whole id spelled other than it is stored; the
// id under the other kind of post; and another post's id. Under the post's own issue an id
// written short is that issue's, and the read is refreshed as every address of the issue is.
test("a correction leaves alone a names read naming the post by no form the server reads as the post", () => {
  const short = POST.slice(0, 8);
  const own = [[`dispatch://CORE-1/comment/${short}`]];
  const none = [
    [`dispatch://CORE-2/comment/${short}`],
    [`dispatch://CORE-2/comment/${POST.slice(0, 13)}`],
    [`dispatch://CORE-2/comment/${POST.toUpperCase()}`],
    [`dispatch://CORE-2/comment/${POST.replaceAll("-", "")}`],
    [`dispatch://CORE-2/message/${POST}`],
    [`dispatch://CORE-2/ask/${POST}`],
    [`dispatch://CORE-12/comment/${OTHER}`],
    [`dispatch://ACME/artifact/roadmap-md/comment/${short}`],
  ];
  for (const type of ["comment.edited", "comment.deleted"] as const) {
    expect(refreshedNames(correction(type), [...own, ...none]), type).toEqual(own);
  }
  // Only a post's correction reads a post's id: an ask's edit refreshes what is under its issue.
  expect(
    refreshedNames(correction("ask.edited", { anchor: null }), [[`dispatch://CORE-2/ask/${POST}`]])
  ).toEqual([]);
});

/** A names read's answer for `ref`, a comment's member unless `fields` says otherwise. */
function member(ref: string, fields: Partial<ReferenceTarget> = {}): ReferenceTarget {
  return { gone: false, kind: "comment", name: "Hold the deploy.", ref, ...fields };
}

const tree = { content: [], type: "doc" };
const ANSWER = [
  member(`dispatch://CORE-2/comment/${POST}`),
  member(`dispatch://CORE-2/comment/${POST}#b-b1`, {
    kind: "block",
    name: "The first block.",
    text_tree: tree,
  }),
  member(`dispatch://CORE-1/comment/${POST.slice(0, 8)}`),
  member(`dispatch://CORE-2/comment/${POST.slice(0, 8)}`, { name: "Another CORE-2 comment." }),
  member(`dispatch://CORE-1/comment/${POST.slice(0, 9)}`, {
    ambiguous: true,
    gone: true,
    name: "",
  }),
  member(`dispatch://CORE-12/comment/${OTHER}`, { name: "Another post." }),
  member(`dispatch://CORE-2/message/${POST}`, { kind: "message", name: "A message." }),
];
const KEY = referenceTargetsQuery(ANSWER.map((target) => target.ref)).queryKey;
const UNRELATED = referenceTargetsQuery([`dispatch://CORE-12/comment/${OTHER}`]).queryKey;

/** The answers held for `KEY` and `UNRELATED` once `event` is written into a cache holding them,
 *  with `UNRELATED` marked stale by an earlier event, and whether it still is. */
function marked(event: DispatchEvent) {
  const client = new QueryClient();
  client.setQueryData(KEY, { targets: ANSWER });
  const unrelated = { targets: [member(`dispatch://CORE-12/comment/${OTHER}`)] };
  client.setQueryData(UNRELATED, unrelated);
  void client.invalidateQueries({ exact: true, queryKey: UNRELATED, refetchType: "none" });
  markCorrectedPost(client, event);
  return {
    targets: client.getQueryData<{ targets: ReferenceTarget[] }>(KEY)?.targets,
    unrelated: {
      same: client.getQueryData(UNRELATED) === unrelated,
      stale: client.getQueryState(UNRELATED)?.isInvalidated,
    },
  };
}

// A delete's member names the post with no words beside the delete's mark, and a block of the
// post is gone, with no words and no tree, as the server answers a deleted post; an id written
// short under the post's own issue is the post. What names no post - a short id under another
// owner, an ambiguous one, another post, the other kind - is left as it was, and so is a read
// naming none of them, its stale mark included.
test("a delete writes its mark into each kept member naming the post, and no other", () => {
  const deletion = { deleted_at: "2026-11-19T00:00:00Z", deleted_by: ALICE };
  const [whole, block, own, ...untouched] = ANSWER;
  expect(marked(correction("comment.deleted", deletion))).toEqual({
    targets: [
      { ...whole, ...deletion, name: "" },
      { gone: true, kind: "block", name: "", ref: block?.ref ?? "", ...deletion },
      { ...own, ...deletion, name: "" },
      ...untouched,
    ],
    unrelated: { same: true, stale: true },
  });
  // A delete whose actor the row does not record carries the time alone, as its read does.
  const anonymous = marked(correction("comment.deleted", { ...deletion, deleted_by: null }));
  expect(anonymous.targets?.[0]).toEqual({ ...whole, deleted_at: deletion.deleted_at, name: "" });
});

// An edit's member, and its block's, carry the edit's time, and keep their words until a read
// gives the new ones.
test("an edit writes its time into each kept member naming the post, and no other", () => {
  const editedAt = "2026-11-19T00:00:00Z";
  const [whole, block, own, ...untouched] = ANSWER;
  expect(marked(correction("comment.edited", { edited_at: editedAt }))).toEqual({
    targets: [
      { ...whole, edited_at: editedAt },
      { ...block, edited_at: editedAt } as ReferenceTarget,
      { ...own, edited_at: editedAt },
      ...untouched,
    ],
    unrelated: { same: true, stale: true },
  });
  // A message's correction marks the message's members, and leaves the comment's alone.
  const message = marked(correction("message.edited", { edited_at: editedAt }));
  expect(message.targets?.map((target) => target.edited_at ?? null)).toEqual([
    null,
    null,
    null,
    null,
    null,
    null,
    editedAt,
  ]);
});
