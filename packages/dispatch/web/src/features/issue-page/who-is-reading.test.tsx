import { expect, type Mock, spyOn, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";
import * as ts from "typescript";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { whoAmIQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { stateForIssue } from "../issue/pins";
import { MarginProvider } from "../margin/margin-context";
import { issue as marginIssue } from "../margin/margin-fixture";
import { buildDispatchReference } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { emptyDetailsReads } from "./details-reads-fixture";
import { FocusedNodeFacts } from "./FocusedNodeFacts";
import type { IssueField } from "./IssueFieldForms";
import { IssueMarginPanel } from "./IssueMargin";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { StagedDocument } from "./StagedDocument";
import { ProvidersSlot } from "./slots/providers";
import { ViewRowSlot } from "./slots/view-row";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";
import { useWhoIsReading } from "./useIssueTree";
import { appClient } from "./window-focus-fixture";

/**
 * Who is reading the new page (`["whoami"]`) is read through one hook, `useIssueTree.ts`'s
 * `useWhoIsReading`, which keeps it fresh by events (`EVENT_FRESH`) and says why once
 * (dispatch://INFRA-3940 R12, INV-6). `choice.ts` is the one file of the page that reads it
 * itself: today's pages call it too (`app.tsx`'s `useIssuePageChoice`), and they keep the app's
 * defaults.
 */

/** The page's own files: each file of this directory but its tests and their fixtures. */
const pageFiles = readdirSync(import.meta.dir, { encoding: "utf8", recursive: true })
  .filter((path) => /\.tsx?$/.test(path) && !/\.test\.tsx?$/.test(path) && !/fixture/.test(path))
  .sort();

/** The file that reads who is reading as today's pages do, which no hook of the new page may
 *  change. */
const TODAYS_CHOOSER = "choice.ts";

/** The name of the function declared nearest around `node`, or `undefined` at a module's top. */
function declaredAround(node: ts.Node): string | undefined {
  for (let at = node.parent; at !== undefined; at = at.parent) {
    if (ts.isFunctionDeclaration(at)) return at.name?.text;
    if (ts.isFunctionLike(at)) return undefined;
  }
  return undefined;
}

/** Each call of `whoAmIQuery` in `source`, as `<path>:<line>` with the function it is called in. */
function whoAmIReads(path: string, source: string): { inside?: string; place: string }[] {
  const file = ts.createSourceFile(path, source, ts.ScriptTarget.Latest, true);
  const reads: { inside?: string; place: string }[] = [];
  const visit = (node: ts.Node): void => {
    if (
      ts.isCallExpression(node) &&
      ts.isIdentifier(node.expression) &&
      node.expression.text === "whoAmIQuery"
    ) {
      const { line } = file.getLineAndCharacterOfPosition(node.getStart(file));
      reads.push({ inside: declaredAround(node), place: `${path}:${line + 1}` });
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return reads;
}

test("no file of the new page reads whoami but through useWhoIsReading", () => {
  const outside: string[] = [];
  let inHook = 0;
  for (const path of pageFiles) {
    if (path === TODAYS_CHOOSER) continue;
    for (const read of whoAmIReads(path, readFileSync(join(import.meta.dir, path), "utf8"))) {
      if (path === "useIssueTree.ts" && read.inside === "useWhoIsReading") {
        inHook += 1;
      } else {
        outside.push(read.place);
      }
    }
  }
  expect(outside, "the files that read who is reading themselves").toEqual([]);
  expect(inHook, "the reads of who is reading inside useWhoIsReading").toBe(1);
});

/** Who is reading, as the shell's read answered it. */
const READER = { event_head: "0", kind: "user", login: "alice" } as const;

/** The fixtures' issue, open. */
const ISSUE: IssueDetails = { ...marginIssue, status: "todo" };

/** The issue's route, as the page's slots are handed it. */
const ROUTE = { key: ISSUE.key, kind: "issue" } as const;

/** An open Details drawer, as the new page's shell hands the margin one. */
const OPEN_DRAWER: MarginDrawer = { open: true, setOpen: () => {}, setPage: () => {} };

answerTextReadsWithTheSeededText();

/**
 * The app's client (`appClient`: a read is stale 30 s after it answered, as `main.tsx` has it),
 * holding who is reading as the shell answered it 31 s ago. Staleness is per observer, so a reader
 * under the app's defaults asks for it again as it mounts, and one kept fresh by events does not.
 */
function answeredPastStaleness(): QueryClient {
  const client = appClient();
  client.setQueryData(whoAmIQuery().queryKey, READER, { updatedAt: Date.now() - 31_000 });
  return client;
}

/** The read of who is reading, counted: an answer that never comes, so no answer draws anything
 *  again after the count. */
function countWhoIsReading(): Mock<typeof api.whoAmI> {
  return spyOn(api, "whoAmI").mockReturnValue(Promise.withResolvers<never>().promise);
}

/** How many times who is reading is asked as `node` is drawn under `client`'s answers. A read the
 *  mount asks for is sent from its observer's effect, which `render` runs: a flush of effects and
 *  microtasks after it sends any not yet sent. */
async function askedWhileDrawing(
  who: Mock<typeof api.whoAmI>,
  client: QueryClient,
  node: ReactNode
): Promise<number> {
  who.mockClear();
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${ISSUE.key}`]}>
      <QueryClientProvider client={client}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await act(async () => {});
    return who.mock.calls.length;
  } finally {
    view.unmount();
  }
}

/** Who is reading as a reader of the page takes it: the hook alone. */
function ReadsWhoIsReading(): ReactNode {
  useWhoIsReading();
  return null;
}

test("a who-is-reading answer 31 seconds old sends no request", async () => {
  const who = countWhoIsReading();
  try {
    expect(await askedWhileDrawing(who, answeredPastStaleness(), <ReadsWhoIsReading />)).toBe(0);
  } finally {
    who.mockRestore();
  }
});

/** Details' properties as the details slot draws them (`slots/details.tsx`): the panel over its
 *  own set of the issue's writes. */
function DetailsProperties(): ReactNode {
  const writes = useIssueHeaderWrites(ISSUE, stateForIssue(undefined, ISSUE.key));
  const [editingField, setEditingField] = useState<IssueField | null>(null);
  return (
    <IssueMarginPanel
      documentArtifact={primarySpec(ISSUE)}
      editingField={editingField}
      isClosed={false}
      issue={ISSUE}
      onEditingField={setEditingField}
      state={undefined}
      writes={writes}
    />
  );
}

test("a who-is-reading answer 31 seconds old sends no request: Details' properties", async () => {
  const reads = emptyDetailsReads();
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(ISSUE);
  // What references the issue and what waits on it: nothing.
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({
      id: ISSUE.key,
      issue_key: ISSUE.key,
      kind: "issue",
      ref: buildDispatchReference({ key: ISSUE.key, kind: "issue" }),
    }),
  });
  const who = countWhoIsReading();
  try {
    const asked = await askedWhileDrawing(
      who,
      answeredPastStaleness(),
      <MarginProvider>
        <MarginSlotProvider drawer={OPEN_DRAWER}>
          <ProvidersSlot issue={ISSUE} route={ROUTE}>
            <DetailsProperties />
          </ProvidersSlot>
        </MarginSlotProvider>
      </MarginProvider>
    );
    expect(asked, "Details asked who is reading").toBe(0);
  } finally {
    who.mockRestore();
    getReferences.mockRestore();
    getIssue.mockRestore();
    reads.restore();
  }
});

test("a who-is-reading answer 31 seconds old sends no request: the view row", async () => {
  // The reads of the issue's tree, which the row counts from, each answered empty.
  const reads = [
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
    spyOn(api, "getIssueResults").mockResolvedValue({ checks: [] } as never),
  ];
  const who = countWhoIsReading();
  try {
    const asked = await askedWhileDrawing(
      who,
      answeredPastStaleness(),
      <KeymapProvider>
        <ProvidersSlot issue={ISSUE} route={ROUTE}>
          <ViewRowSlot issue={ISSUE} route={ROUTE} />
        </ProvidersSlot>
      </KeymapProvider>
    );
    expect(asked, "the view row asked who is reading").toBe(0);
  } finally {
    who.mockRestore();
    for (const read of reads) read.mockRestore();
  }
});

test("a who-is-reading answer 31 seconds old sends no request: Details' facts of the focused row", async () => {
  // The outline's reads the row's controls are drawn from, answered empty.
  const reads = [
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
  ];
  const who = countWhoIsReading();
  try {
    const asked = await askedWhileDrawing(
      who,
      answeredPastStaleness(),
      <ProvidersSlot issue={ISSUE} route={ROUTE}>
        <FocusedNodeFacts
          issueKey={ISSUE.key}
          issueTitle={ISSUE.title}
          node={{
            id: "comment-1",
            item: { id: "comment-1", kind: "comment" },
            text: "Ship it on Friday.",
            type: "comment",
          }}
          state={undefined}
        />
      </ProvidersSlot>
    );
    expect(asked, "the focused row's facts asked who is reading").toBe(0);
  } finally {
    who.mockRestore();
    for (const read of reads) read.mockRestore();
  }
});

test("a who-is-reading answer 31 seconds old sends no request: the staged document", async () => {
  answerRequests(...documentSurfaceHandlers());
  const who = countWhoIsReading();
  try {
    const asked = await askedWhileDrawing(
      who,
      answeredPastStaleness(),
      <DocumentRuntime.Provider value={fakeDocumentRuntime({ text: "The live document" }).runtime}>
        <MarginProvider>
          <StagedDocument
            artifact={artifact}
            isClosed={false}
            issueKey={ISSUE.key}
            onToolbarChange={() => {}}
            onVersionChange={() => {}}
            showDiff={false}
          />
        </MarginProvider>
      </DocumentRuntime.Provider>
    );
    expect(asked, "the staged document asked who is reading").toBe(0);
  } finally {
    who.mockRestore();
  }
});
