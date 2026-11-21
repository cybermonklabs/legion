// What the artifact page's header keeps in its box while the editor's first batch has not come
// (dispatch://INFRA-341 R11, `features/doc/AfterTheFirstBatch.tsx`). The chrome itself waits, as
// it is meant to; the two things in it that tell a reader something no press of theirs would ask
// for - the document's connection and what a review of it decided - stand there from the first
// frame, because a reader cannot reach for a notice they were never given.
//
// And what the hold must not take from the reader: the landing highlight, which is two seconds
// of ring on the chrome that waits, so it is counted from the chrome rather than from the box.
import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { onFakeClock, tick } from "../../__tests__/fake-clock";
import type { Artifact, ArtifactApproval } from "../../api/types";
import { highlightRing } from "../../theme/classes";
import type { ConnectionState } from "../doc/connection";
import { theFirstBatchIsAwaited } from "../doc/first-batch";
import type { DocumentToolbar } from "../doc/ProofDocument";
import type { PendingState } from "../doc/pending-sync";
import { ArtifactHeader } from "./ArtifactHeader";

afterEach(() => {
  cleanup();
  theFirstBatchIsAwaited(false);
});

const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  ref_key: "CORE-1/spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

function toolbarWith(connection: ConnectionState, pending: PendingState | undefined) {
  return {
    connection,
    copyBlockLink: async () => true,
    isNamingVersion: false,
    pending,
    requestNamedVersion: () => {},
    versions: [],
  } satisfies DocumentToolbar;
}

function renderHeader(target: Artifact, toolbar: DocumentToolbar | undefined, highlight = false) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ArtifactHeader
          artifact={target}
          highlight={highlight}
          isClosed={false}
          showDiff={false}
          showVersionPicker
          toolbar={toolbar}
          version={undefined}
        >
          <p>the document</p>
        </ArtifactHeader>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("the artifact header's box says the document is not connected and holds an unsent edit", () => {
  theFirstBatchIsAwaited(true);
  const view = renderHeader(
    artifact,
    toolbarWith("connecting", { count: 1, readOnly: false, rebuiltAt: undefined, stored: true })
  );

  try {
    // The chrome is held, as the hold intends: no version picker, no Name version, no copy.
    expect(screen.queryByTestId("artifact-header")).toBeNull();
    expect(screen.queryByRole("button", { name: "Name version" })).toBeNull();
    // The dot and its notice are not, because nothing a reader presses would fetch them.
    const dot = screen.getByRole("status");
    expect(dot.textContent).toContain("1 edit saved in this browser, not sent yet.");
    expect(dot.getAttribute("title")).toBe(
      "Connecting to the document… · 1 edit saved in this browser, not sent yet."
    );

    // And one dot, not two, once the batch lets the header through.
    act(() => theFirstBatchIsAwaited(false));
    expect(screen.getByTestId("artifact-header")).toBeTruthy();
    expect(screen.getAllByRole("status")).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("the artifact header's box says what a review of the document decided, and its actions wait", () => {
  const approval: ArtifactApproval = { latest_version: 3, state: "changes_requested", version: 3 };
  theFirstBatchIsAwaited(true);
  const view = renderHeader({ ...artifact, approval }, toolbarWith("connected", undefined));

  try {
    expect(screen.queryByTestId("artifact-header")).toBeNull();
    // The state as text, never as a control: a press inside this box replaces the box with the
    // header it keeps, in that same press, so a chip drawn here as a button would swallow the
    // press it was drawn for and open nothing.
    expect(screen.getByText("Changes requested")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Changes requested" })).toBeNull();
    // Approve and Request changes are controls, and a press on the box is how a reader asks for
    // a control: they arrive with the rest of the chrome.
    expect(screen.queryByRole("button", { name: "Request changes" })).toBeNull();

    act(() => theFirstBatchIsAwaited(false));
    expect(screen.getByRole("button", { name: "Request changes" })).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Changes requested" })).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

// The landing highlight is the mark that answers "which of these is the artifact I asked for"
// (`ArtifactRoutePanel.tsx` arms it on the artifact an address names), and it is two seconds
// long. The chrome that wears it is held behind the first batch, so a first batch slower than
// two seconds would spend every one of them behind the hold and the reader would be shown
// nothing: this row holds the chrome back past the two seconds and reads the ring after it.
test("the landing highlight's two seconds run from the chrome, not from the box that waits", async () => {
  await onFakeClock(async () => {
    theFirstBatchIsAwaited(true);
    const view = renderHeader(artifact, toolbarWith("connected", undefined), true);

    try {
      // A first batch that takes longer than the highlight itself.
      await tick(3_000);
      expect(screen.queryByTestId("artifact-header")).toBeNull();

      // The chrome lands, and the reader gets the mark they navigated for in its first frame.
      act(() => theFirstBatchIsAwaited(false));
      const header = screen.getByTestId("artifact-header");
      expect(header.className).toContain(highlightRing);
      expect(header.getAttribute("data-landing-ring")).toBe("");

      // Two seconds of it, counted from there and no shorter.
      await tick(1_900);
      expect(header.className).toContain(highlightRing);
      expect(header.getAttribute("data-landing-ring")).toBe("");

      // And gone at the end of them, as it is on a page whose chrome never waited.
      await tick(200);
      expect(header.className).not.toContain(highlightRing);
      expect(header.getAttribute("data-landing-ring")).toBeNull();
    } finally {
      view.unmount();
    }
  });
});
