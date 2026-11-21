import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, type QueryKey, useQuery } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { correction, correctionFeed } from "../__tests__/correction-feed";
import { useEventFeed } from "./event-feed";
import { whoAmIQuery } from "./queries";
import type { Artifact, ArtifactVersionText, DispatchEvent, IssueDetails } from "./types";

// The architect's ruling of 2026-11-20 09:18Z on dispatch://INFRA-1523: an accept names the
// document version it makes after the comment's body, and the comment's delete renames that
// version on the server (`renameAcceptedVersion` in `api/comment_delete.go`). Every cached answer
// that lists the document's versions draws that name - the issue's details (`["issue", key]`), the
// document's own read (`["artifact", id]`) and a project document's read by its slug
// (`["artifact-ref", …]`) - and so does each read of that version alone; none shows an error when
// its refetch fails, so each kept the comment's words as the version's name. The delete's event
// names the version and the name it took (`renamed_version`), and the feed writes that name into
// each (`markCorrectedPost`).

const COMMENT = "6f7a8b9c-0d1e-4f2a-8b3c-4d5e6f7a8b9c";
const DOCUMENT = "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d";
/** The comment's words, which the accept of its suggestion named version 2 after. */
const WORDS = "zarquonversionbody";
const NAMED_AFTER_THE_BODY = `Say it slower, ${WORDS}.`;
/** The name the delete gave version 2: the accepted suggestion's quote and replacement. */
const RENAMED = 'Accepted suggestion: "quick" → "slow"';

/** The document as a read lists it before the delete: version 2, the accept's, named after the
 *  comment's body. */
function documentRead(more: Partial<Artifact> = {}): Artifact {
  return {
    created_at: "2026-11-19T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: DOCUMENT,
    issue_key: "CORE-1",
    kind: "doc",
    name: "spec.md",
    primary: true,
    project: "CORE",
    ref_key: "CORE-1/spec",
    session_id: null,
    slug: "spec",
    versions: [
      { authors: [], created_at: "2026-11-19T00:00:00Z", named: false, number: 1, summary: null },
      {
        authors: [],
        created_at: "2026-11-19T01:00:00Z",
        named: true,
        number: 2,
        summary: NAMED_AFTER_THE_BODY,
      },
    ],
    ...more,
  } as Artifact;
}

/** The comment's delete as the feed carries it: the comment anchored on the document, and the
 *  version the delete renamed. */
function deleteEvent(payload: Record<string, unknown> = {}): DispatchEvent {
  return correction("comment", COMMENT, false, {
    anchor: {
      artifact_id: DOCUMENT,
      block_id: null,
      mark_id: COMMENT,
      orphaned: true,
      quote: "quick",
      version: 1,
    },
    renamed_version: { artifact_id: DOCUMENT, number: 2, summary: RENAMED },
    ...payload,
  });
}

/** The name of each version `document` lists, in its order. */
const versionNames = (document: Pick<Artifact, "versions"> | undefined) =>
  document?.versions?.map((version) => version.summary);

/**
 * Caches `answer` under `queryKey` through a read whose every refetch fails, delivers `event` on
 * the page's event feed (`correctionFeed`), and answers the version names `names` reads of the
 * cached answer before the event and after the refetch the event sent has failed, with the whole
 * answer then as text.
 */
async function namesAfterTheDelete<T>(
  queryKey: QueryKey,
  answer: T,
  event: DispatchEvent,
  names: (answer: T | undefined) => unknown
): Promise<{ readonly before: unknown; readonly after: unknown; readonly kept: string }> {
  let reads = 0;
  const fetchAnswer = (): Promise<T> => {
    reads += 1;
    return reads === 1 ? Promise.resolve(answer) : Promise.reject(new Error("the read failed"));
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const feed = correctionFeed(client);
  const hook = renderHook(
    () => {
      useEventFeed();
      return useQuery({ queryFn: fetchAnswer, queryKey });
    },
    {
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client }, children),
    }
  );
  try {
    await waitFor(() => expect(hook.result.current.data).toBeDefined());
    const before = names(client.getQueryData<T>(queryKey));
    feed.start();
    await feed.deliver(event, [queryKey]);
    expect(reads).toBe(2);
    return {
      after: names(client.getQueryData<T>(queryKey)),
      before,
      kept: JSON.stringify(client.getQueryData(queryKey)),
    };
  } finally {
    hook.unmount();
    feed.restore();
    client.clear();
  }
}

test("a version a comment's delete renamed takes its new name in the issue's details, whose refetch fails", async () => {
  const issue = {
    artifacts: [documentRead()],
    key: "CORE-1",
    updated_at: "2026-11-19T01:00:00Z",
  } as unknown as IssueDetails;
  const read = await namesAfterTheDelete(["issue", "CORE-1"], issue, deleteEvent(), (cached) =>
    versionNames(cached?.artifacts?.[0])
  );
  expect({ after: read.after, before: read.before }).toEqual({
    after: [null, RENAMED],
    before: [null, NAMED_AFTER_THE_BODY],
  });
  expect(read.kept).not.toContain(WORDS);
});

test("a version a comment's delete renamed takes its new name in the document's own read, whose refetch fails", async () => {
  const read = await namesAfterTheDelete(
    ["artifact", DOCUMENT],
    documentRead(),
    deleteEvent(),
    versionNames
  );
  expect({ after: read.after, before: read.before }).toEqual({
    after: [null, RENAMED],
    before: [null, NAMED_AFTER_THE_BODY],
  });
  expect(read.kept).not.toContain(WORDS);
});

// A project document is read by its project and slug, and its comment's events belong to no
// issue: the event names the document instead (`artifact_id`).
test("a version a comment's delete renamed takes its new name in a project document's read by its slug, whose refetch fails", async () => {
  const project = documentRead({
    issue_key: null,
    name: "notes.md",
    ref_key: "CORE/notes",
    slug: "notes",
  });
  const event = {
    ...deleteEvent({ artifact_id: DOCUMENT, issue_key: null }),
    artifact_id: DOCUMENT,
    issue_key: null,
  } as unknown as DispatchEvent;
  const read = await namesAfterTheDelete(
    ["artifact-ref", "CORE/notes"],
    project,
    event,
    versionNames
  );
  expect({ after: read.after, before: read.before }).toEqual({
    after: [null, RENAMED],
    before: [null, NAMED_AFTER_THE_BODY],
  });
  expect(read.kept).not.toContain(WORDS);
});

// A read of one version - the version view's and the compare's (`["artifact", id, "version", n]`),
// and a reference's pinned version (`artifactTextQuery`, `["artifact", id, n]`) - carries that
// version's name beside its text.
test("a version a comment's delete renamed takes its new name in each read of that version alone, whose refetch fails", async () => {
  const version = {
    authors: [],
    created_at: "2026-11-19T01:00:00Z",
    markdown: "The slow brown fox\n",
    named: true,
    number: 2,
    summary: NAMED_AFTER_THE_BODY,
  } as ArtifactVersionText;
  for (const queryKey of [
    ["artifact", DOCUMENT, "version", 2],
    ["artifact", DOCUMENT, 2],
  ]) {
    const read = await namesAfterTheDelete(
      queryKey,
      version,
      deleteEvent(),
      (cached) => cached?.summary
    );
    expect({ after: read.after, before: read.before, queryKey }).toEqual({
      after: RENAMED,
      before: NAMED_AFTER_THE_BODY,
      queryKey,
    });
    expect(read.kept).not.toContain(WORDS);
  }
});
