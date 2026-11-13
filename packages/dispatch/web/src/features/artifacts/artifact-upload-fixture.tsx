import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactNode } from "react";

import { ArtifactDropZone, ArtifactUploadRow, useArtifactUpload } from "./ArtifactUpload";

// The upload row `ArtifactUpload.test.tsx` and `ArtifactUpload.stopped.test.tsx` both render,
// and what Dispatch answers their uploads.

function TestHarness(): ReactNode {
  const upload = useArtifactUpload({ issue: "CORE-1" });
  return (
    <ArtifactDropZone upload={upload}>
      <ArtifactUploadRow upload={upload} />
    </ArtifactDropZone>
  );
}

/** `ui` under a query client of its own, which the cases that stop a late write reach through
 *  the returned `queryClient` as the shell's indicator does. */
export function renderWith(ui: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return {
    ...render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>),
    queryClient,
  };
}

export function renderHarness() {
  return renderWith(<TestHarness />);
}

/** What Dispatch answers both files' uploads, whose own fields none of them reads. */
export const UPLOADED = {
  artifact: {
    created_at: "2026-09-11T00:00:00Z",
    created_by: { id: "alice", kind: "user" as const },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "doc" as const,
    name: "notes.md",
    primary: false,
    project: "CORE",
    ref_key: "CORE-1/notes-md",
    session_id: null,
    slug: "notes-md",
    versions: [],
  },
  version: {
    authors: [{ id: "alice", kind: "user" as const }],
    created_at: "2026-09-11T00:00:00Z",
    named: false,
    number: 2,
    summary: null,
  },
};
