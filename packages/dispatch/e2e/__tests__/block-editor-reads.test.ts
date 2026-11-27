import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { api, documentReadRequest } from "../../web/src/api/client";
import {
  artifactBlocksQuery,
  issueAsksQuery,
  issueAskThreadsQuery,
  issueViewOfQuery,
  whoAmIQuery,
} from "../../web/src/api/queries";
import {
  liveTextQueryOptions,
  ownerAsksQueryOptions,
} from "../../web/src/features/doc/ProofDocument";
import { issueDecisionsQuery } from "../../web/src/features/doc/spec-marks";
import {
  BLOCK_EDITOR_READY_MARK,
  BLOCK_EDITOR_START_MARK,
  BLOCK_EDITOR_TASK_MEASURE,
  FORBIDDEN_READS,
  isForbiddenRead,
  isRoomSocket,
  ROOM_SOCKET_PATTERN,
} from "../block-editor";

const SAMPLE_KEY = "LAY-1";
const SAMPLE_ARTIFACT_ID = "0b4f6c1e-9d2a-4e7b-8c35-2f1a6d9e7b40";

/** Runs `invoke` with `globalThis.fetch` intercepted so every outgoing request's path+search is
 *  recorded and answered with minimal valid JSON, then restores the original `fetch`. */
async function captureRequestedPaths(invoke: () => Promise<unknown>): Promise<string[]> {
  const requested: string[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: string | URL | Request): Promise<Response> => {
    const raw = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    const url = new URL(raw, "http://dispatch.local");
    requested.push(`${url.pathname}${url.search}`);
    return new Response(JSON.stringify({ entries: [], nodes: [], version: null, view_of: null }), {
      headers: { "content-type": "application/json" },
      status: 200,
    });
  }) as typeof fetch;
  try {
    await invoke();
  } catch {
    // Some queryFn callers validate response shapes after fetch; the captured request path is what
    // this inventory checks.
  } finally {
    globalThis.fetch = originalFetch;
  }
  return requested;
}

test("the forbidden reads match every read the base's document editor makes of a document's text, blocks, decisions, read marker or asks", async () => {
  expect(BLOCK_EDITOR_START_MARK).toBe("block-editor-start");
  expect(BLOCK_EDITOR_READY_MARK).toBe("block-editor-ready");
  expect(BLOCK_EDITOR_TASK_MEASURE).toBe("block-editor-task");
  expect(ROOM_SOCKET_PATTERN.test(`/ws/doc/${SAMPLE_ARTIFACT_ID}`)).toBe(true);
  expect(isRoomSocket(`wss://example.test/ws/doc/${SAMPLE_ARTIFACT_ID}?schema_version=7`)).toBe(
    true
  );
  expect(isRoomSocket("wss://example.test/ws/live")).toBe(false);

  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  // Every read `ProofDocument.tsx` and `useSpecMarks` make for the whole-document editor, plus
  // Conversation's asks-thread read which must not be re-sent during block-editor readiness:
  const forbiddenFactories: readonly {
    readonly label: string;
    readonly run: () => Promise<unknown>;
  }[] = [
    {
      label: "liveTextQueryOptions (artifact text)",
      run: () => client.fetchQuery(liveTextQueryOptions(SAMPLE_ARTIFACT_ID)),
    },
    {
      label: "artifactBlocksQuery (artifact blocks)",
      run: () => client.fetchQuery(artifactBlocksQuery(SAMPLE_ARTIFACT_ID)),
    },
    {
      label: "ownerAsksQueryOptions (artifact asks)",
      run: () => client.fetchQuery(ownerAsksQueryOptions(SAMPLE_ARTIFACT_ID, null)),
    },
    {
      label: "ownerAsksQueryOptions (issue asks)",
      run: () => client.fetchQuery(ownerAsksQueryOptions(SAMPLE_ARTIFACT_ID, SAMPLE_KEY)),
    },
    {
      label: "issueAsksQuery (issue asks)",
      run: () => client.fetchQuery(issueAsksQuery(SAMPLE_KEY)),
    },
    {
      label: "issueAskThreadsQuery (asks?state=all&thread=1)",
      run: () => client.fetchQuery(issueAskThreadsQuery(SAMPLE_KEY)),
    },
    {
      label: "issueViewOfQuery (tree?view=spec)",
      run: () => client.fetchQuery(issueViewOfQuery(SAMPLE_KEY)),
    },
    {
      label: "issueDecisionsQuery (tree?view=decisions)",
      run: () => client.fetchQuery(issueDecisionsQuery(SAMPLE_KEY, true)),
    },
    {
      label: "getDocumentRead (read marker)",
      run: () => api.getDocumentRead(SAMPLE_ARTIFACT_ID),
    },
    {
      label: "documentReadRequest (read marker write)",
      run: async () => {
        const req = documentReadRequest(SAMPLE_ARTIFACT_ID, 1);
        await fetch(req.path);
      },
    },
  ];

  for (const factory of forbiddenFactories) {
    const paths = await captureRequestedPaths(factory.run);
    expect(paths.length, `${factory.label} sent no request`).toBeGreaterThan(0);
    for (const path of paths) {
      expect(
        isForbiddenRead(path),
        `FORBIDDEN_READS did not match ${factory.label} (${path})`
      ).toBe(true);
    }
  }

  // Allowed reads that FORBIDDEN_READS must NOT match:
  const allowedFactories: readonly {
    readonly label: string;
    readonly run: () => Promise<unknown>;
  }[] = [
    {
      label: "Conversation tree read (no view param)",
      run: () => api.getIssueTree(SAMPLE_KEY, { kind: ["conversation"] }),
    },
    {
      label: "Graph tree read (no view param)",
      run: () => api.getIssueTree(SAMPLE_KEY),
    },
    {
      label: "whoami",
      run: () => client.fetchQuery(whoAmIQuery()),
    },
  ];

  for (const factory of allowedFactories) {
    const paths = await captureRequestedPaths(factory.run);
    expect(paths.length, `${factory.label} sent no request`).toBeGreaterThan(0);
    for (const path of paths) {
      expect(
        isForbiddenRead(path),
        `FORBIDDEN_READS wrongly matched ${factory.label} (${path})`
      ).toBe(false);
    }
  }

  expect(FORBIDDEN_READS.length).toBeGreaterThan(0);
});
