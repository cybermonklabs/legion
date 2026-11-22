import { expect, test } from "bun:test";
import {
  changedReview,
  ghReview,
  type Kept,
  keptRead,
  latestReviews,
  type ReviewCall,
  type ReviewEdit,
  sentReview,
  threads,
} from "./review-keep";

const HEAD = "9f1a2b3c4d5e6f708192a3b4c5d6e7f809a1b2c3";
/** The template of each review the calls below send. */
const TEMPLATE = {
  id: 7001,
  comment_id: 7101,
  commit_id: HEAD,
  user: { login: "rig[bot]" },
  author_association: "NONE",
  html_url: "https://github.com/example/widgets/pull/1000",
  pull_request_url: "https://api.github.com/repos/example/widgets/pulls/1000",
  added_lines: { "a.ts": 3 },
};
const SENT_AT = "2026-03-04T10:00:00Z";
const NONE: Kept = { reviews: [], comments: [] };

/** A `gh api` POST of a review: its `-f`/`-F` fields, or the text of its `--input` file. */
const api = (fields: Record<string, unknown>, input?: object): ReviewCall => ({
  prReview: false,
  events: [],
  body: undefined,
  input: input === undefined ? undefined : JSON.stringify(input),
  fields,
});
/** A `gh pr review` with the event flags it names and its body. */
const prReview = (events: string[], body?: string): ReviewCall => ({
  prReview: true,
  events,
  body,
  input: undefined,
  fields: {},
});
/** What the run keeps once `call` is sent over `kept`, or null where the rig cannot state it. */
function send(kept: Kept, call: ReviewCall): Kept | null {
  const sent = sentReview(TEMPLATE, call, kept, SENT_AT);
  return sent === null
    ? null
    : { reviews: [...kept.reviews, sent.review], comments: [...kept.comments, ...sent.comments] };
}
/** A read through a route of `mode`, with the call's `id` group and `--json` fields. */
const read = (
  kept: Kept,
  mode: "list" | "one" | "pr" | "comments" | "review_comments",
  call: { id?: string; fields?: string[]; commented?: boolean } = {},
  stdout: unknown = { number: 1000 }
) =>
  keptRead(
    mode,
    kept,
    { id: call.id, fields: call.fields, commented: call.commented ?? false },
    { stdout }
  );
/** A change through a route of `edit` to the review `id` names, sent as the call's `-f`/`-F`
 * fields, or as the text of its `--input` file. */
const change = (
  kept: Kept,
  edit: ReviewEdit,
  id: string,
  fields: Record<string, unknown>,
  input?: unknown
) =>
  changedReview(edit, kept, {
    id,
    input: input === undefined ? undefined : JSON.stringify(input),
    fields,
  });
/** The review of `kept` at `index`, which the row sent. */
const nth = (kept: Kept, index: number) => kept.reviews[index] as Kept["reviews"][number];

// Catches: a review sent with no event kept as anything but GitHub's pending review (no
// submission time), an event recorded under another state, and a body changed on its way in.
test("a review is kept in the state its event names, with its body as sent, under the next id", () => {
  const pending = send(NONE, api({ body: "draft" }));
  const approved = send(NONE, prReview(["--approve"], "Looks right."));
  const changes = send(NONE, api({}, { event: "REQUEST_CHANGES", body: "Fix it." }));
  expect(
    [pending, approved, changes].map((kept) => {
      const review = kept?.reviews[0];
      return [review?.id, review?.state, review?.body, review?.submitted_at ?? null];
    })
  ).toEqual([
    [7001, "PENDING", "draft", null],
    [7001, "APPROVED", "Looks right.", SENT_AT],
    [7001, "CHANGES_REQUESTED", "Fix it.", SENT_AT],
  ]);
  const second = send(approved as Kept, api({ event: "COMMENT", body: "And this." }));
  expect(second?.reviews.map((review) => [review.id, review.state])).toEqual([
    [7001, "APPROVED"],
    [7002, "COMMENTED"],
  ]);
});

// Catches: a review whose answer the rig cannot state kept rather than refused: a second pending
// review, a review on another commit than the head, a changes request or a comment with no body,
// an event GitHub does not take, `gh pr review` with no event or two, an input that is no JSON
// object, and a pending review that carries comments.
test("a review the rig cannot state is kept nowhere", () => {
  const pending = send(NONE, api({ body: "draft" })) as Kept;
  expect([
    send(pending, api({ body: "again" })),
    send(NONE, api({ event: "APPROVE", commit_id: "0".repeat(40) })),
    send(NONE, api({ event: "REQUEST_CHANGES" })),
    send(NONE, api({ event: "COMMENT", body: "" })),
    send(NONE, api({ event: "DISMISS", body: "x" })),
    send(NONE, prReview([])),
    send(NONE, prReview(["--approve", "--comment"], "x")),
    send(NONE, prReview(["--comment"])),
    send(NONE, api({}, ["not", "an", "object"])),
    send(NONE, api({}, { comments: [{ path: "a.ts", line: 1, body: "y" }] })),
  ]).toEqual(Array(10).fill(null));
});

// Catches: an inline comment off the diff kept rather than refused (another file, a line past the
// file's added lines, the left side, a start line at or after its line, no body), and a kept one
// with another id, review, path, line, body or commit than GitHub records.
test("an inline comment is kept on a line the diff adds, and refused anywhere else", () => {
  const comment = (fields: Record<string, unknown>) =>
    send(NONE, api({}, { event: "COMMENT", body: "x", comments: [{ body: "y", ...fields }] }));
  expect([
    comment({ path: "b.ts", line: 1 }),
    comment({ path: "a.ts", line: 4 }),
    comment({ path: "a.ts", line: 2, side: "LEFT" }),
    comment({ path: "a.ts", line: 2, start_line: 2 }),
    comment({ path: "a.ts", line: 2, body: "" }),
  ]).toEqual(Array(5).fill(null));
  const kept = comment({ path: "a.ts", line: 3, start_line: 1 }) as Kept;
  expect(
    kept.comments.map((c) => [
      c.id,
      c.pull_request_review_id,
      c.path,
      c.start_line,
      c.line,
      c.body,
      c.commit_id,
      c.html_url,
    ])
  ).toEqual([[7101, 7001, "a.ts", 1, 3, "y", HEAD, `${TEMPLATE.html_url}#discussion_r7101`]]);
});

// Catches: a read of what the role sent that answers another review than the one it names, the
// comments of another review, or anything but GitHub's measured 404 for an id no review has, and
// the lists in another order than sent.
test("the reviews and their comments read back by list and by id, and an unknown id is GitHub's 404", () => {
  const first = send(NONE, api({ body: "draft" })) as Kept;
  const kept = send(
    first,
    api({}, { event: "COMMENT", body: "See.", comments: [{ path: "a.ts", line: 3, body: "here" }] })
  ) as Kept;
  expect(read(kept, "list")).toEqual({ stdout: kept.reviews });
  expect(read(kept, "comments")).toEqual({ stdout: kept.comments });
  expect(read(kept, "one", { id: "7002" })).toEqual({ stdout: kept.reviews[1] });
  expect(read(kept, "review_comments", { id: "7002" })).toEqual({ stdout: kept.comments });
  expect(read(kept, "review_comments", { id: "7001" })).toEqual({ stdout: [] });
  expect([
    read(kept, "one", { id: "7003" }),
    read(kept, "review_comments", { id: "6001" }),
  ]).toEqual([{ github: "review-404" }, { github: "review-comments-404" }]);
  expect(read(NONE, undefined as never)).toBeUndefined();
});

// Catches: `gh pr view --json reviews` that loses a review, shows it in REST's shape rather than
// gh's (GraphQL's bare bot login, node id, null submission time while pending), a latestReviews
// the rig cannot name answered rather than refused, and the comments field answered after the
// role posted a comment the stand-in does not keep.
test("a pull request read carries the reviews in gh's fields, and refuses what the rig cannot name", () => {
  const approved = send(NONE, prReview(["--approve"], "Looks right.")) as Kept;
  const view = read(approved, "pr", { fields: ["reviews", "latestReviews"] });
  expect(view).toEqual({
    stdout: {
      number: 1000,
      reviews: [ghReview(approved.reviews[0] as Kept["reviews"][number])],
      latestReviews: latestReviews(approved.reviews),
      comments: [],
    },
  });
  expect(ghReview(approved.reviews[0] as Kept["reviews"][number])).toMatchObject({
    id: "PRR_7001",
    author: { login: "rig" },
    state: "APPROVED",
    submittedAt: SENT_AT,
    commit: { oid: HEAD },
  });
  expect(latestReviews(approved.reviews)?.map((review) => [review.id, review.commit])).toEqual([
    ["", { oid: "" }],
  ]);
  const pending = send(NONE, api({ body: "draft" })) as Kept;
  expect([
    read(pending, "pr", { fields: ["latestReviews"] }),
    read(NONE, "pr", { fields: ["comments"], commented: true }),
  ]).toEqual([null, null]);
  expect(read(pending, "pr", { fields: ["reviews"] })).toMatchObject({
    stdout: { reviews: [{ state: "PENDING", submittedAt: null }] },
  });
  // An answer that is no pull request object is the route's own, unchanged.
  expect(read(NONE, "pr", {}, "text")).toEqual({ stdout: "text" });
});

// INFRA-1303 (trial-i-head-4 of the batch at 585b19a3 updated its review's body and dismissed it,
// and met two refusals). Catches: an update that changes any field but the body, or takes the
// body from anything but the `body` the call sends, by its fields or its --input; and an update
// answered where GitHub's answer is not measured (a review the role did not send, a pending one,
// an empty or missing body, an input that is no JSON object), which the stand-in must refuse.
test("an update sets the body of a review the role sent, and every other field stays", () => {
  const kept = send(NONE, api({}, { event: "APPROVE", body: "Looks right." })) as Kept;
  expect([
    change(kept, "update", "7001", {}, { body: "Looks right; the re-run is green." }),
    change(kept, "update", "7001", { body: "By its field." }),
  ]).toEqual([
    { review: { ...nth(kept, 0), body: "Looks right; the re-run is green." } },
    { review: { ...nth(kept, 0), body: "By its field." } },
  ]);
  const pending = send(NONE, api({ body: "draft" })) as Kept;
  expect([
    change(kept, "update", "7002", { body: "x" }),
    change(kept, "update", "7001", { body: "" }),
    change(kept, "update", "7001", {}),
    change(kept, "update", "7001", {}, ["not", "an", "object"]),
    change(pending, "update", "7001", { body: "x" }),
  ]).toEqual(Array(5).fill(null));
});

// Catches: a dismissal that leaves the state as it was or changes another field, a COMMENTED
// review dismissed where GitHub answers its 422, and a dismissal answered where GitHub's answer is
// not measured (a review the role did not send, no message or an empty one, an event other than
// DISMISS, a review already dismissed, a pending one).
test("a dismissal sets an approval or a changes request DISMISSED, and a comment-only review is GitHub's 422", () => {
  const approved = send(NONE, api({ event: "APPROVE", body: "Looks right." })) as Kept;
  const changes = send(approved, api({ event: "REQUEST_CHANGES", body: "Red." })) as Kept;
  const commented = send(changes, api({ event: "COMMENT", body: "See." })) as Kept;
  expect([
    change(commented, "dismiss", "7001", {}, { message: "Superseded.", event: "DISMISS" }),
    change(commented, "dismiss", "7002", { message: "Superseded." }),
    change(commented, "dismiss", "7003", { message: "Superseded." }),
  ]).toEqual([
    { review: { ...nth(commented, 0), state: "DISMISSED" } },
    { review: { ...nth(commented, 1), state: "DISMISSED" } },
    { github: "review-dismiss-422" },
  ]);
  const dismissed: Kept = { ...approved, reviews: [{ ...nth(approved, 0), state: "DISMISSED" }] };
  const pending = send(NONE, api({ body: "draft" })) as Kept;
  expect([
    change(commented, "dismiss", "7004", { message: "x" }),
    change(commented, "dismiss", "7001", {}),
    change(commented, "dismiss", "7001", { message: "" }),
    change(commented, "dismiss", "7001", { message: "x", event: "APPROVE" }),
    change(dismissed, "dismiss", "7001", { message: "x" }),
    change(pending, "dismiss", "7001", { message: "x" }),
  ]).toEqual(Array(6).fill(null));
});

// Catches: `gh pr view --json` answered with what the rig cannot state once a review is changed:
// gh's mark of an edited review (`includesCreatedEdit`) after an update, and the latest review
// after a dismissal; and a read of the pull request's other fields refused for it.
test("a pull request read refuses the reviews gh would print edited, and the latest once one is dismissed", () => {
  const approved = send(NONE, prReview(["--approve"], "Looks right.")) as Kept;
  const updated: Kept = {
    ...approved,
    reviews: [{ ...nth(approved, 0), body: "Green." }],
    edited: [7001],
  };
  const dismissed: Kept = { ...approved, reviews: [{ ...nth(approved, 0), state: "DISMISSED" }] };
  expect([
    read(updated, "pr", { fields: ["reviews"] }),
    read(updated, "pr", { fields: ["latestReviews"] }),
    read(dismissed, "pr", { fields: ["latestReviews"] }),
  ]).toEqual([null, null, null]);
  expect(read(dismissed, "pr", { fields: ["reviews"] })).toMatchObject({
    stdout: { reviews: [{ state: "DISMISSED", body: "Looks right." }] },
  });
  expect(read(updated, "pr", { fields: ["number"] })).toMatchObject({ stdout: { number: 1000 } });
});

// Catches: threads that lose a kept comment, carry REST's login, or end their page on a cursor
// of another comment.
test("the review threads are the kept inline comments, one unresolved thread each", () => {
  expect(threads([])).toEqual({
    pageInfo: { hasNextPage: false, endCursor: null },
    totalCount: 0,
    nodes: [],
  });
  const kept = send(
    NONE,
    api({}, { event: "COMMENT", body: "See.", comments: [{ path: "a.ts", line: 3, body: "here" }] })
  ) as Kept;
  expect(threads(kept.comments)).toEqual({
    pageInfo: { hasNextPage: false, endCursor: btoa("cursor:v2:7101") },
    totalCount: 1,
    nodes: [
      {
        id: "PRRT_7101",
        isResolved: false,
        comments: { totalCount: 1, nodes: [{ author: { login: "rig" }, body: "here" }] },
      },
    ],
  });
});
