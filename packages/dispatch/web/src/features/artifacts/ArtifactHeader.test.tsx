// The landing highlight on the artifact page's header: the mark that answers "which of these is
// the artifact I asked for" (`ArtifactRoutePanel.tsx` arms it on the artifact an address names),
// two seconds long. `data-landing-ring` stands on the header for exactly as long as the ring is
// painted, and the review layers wait for it to be gone rather than for two seconds of wall clock
// (`e2e/review-routes.ts`'s `waitForRouteSettled`). This holds the attribute to the ring: a mark
// that outlived the ring would stall every one of those waits, and one that ended early would let
// a baseline be taken of a page still ringed.
import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { onFakeClock, tick } from "../../__tests__/fake-clock";
import type { Artifact } from "../../api/types";
import { highlightRing } from "../../theme/classes";
import { ArtifactHeader } from "./ArtifactHeader";

afterEach(cleanup);

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

function renderHeader(highlight: boolean) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ArtifactHeader
          artifact={artifact}
          highlight={highlight}
          isClosed={false}
          showVersionPicker
          version={undefined}
        >
          <p>the document</p>
        </ArtifactHeader>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("the header carries data-landing-ring for as long as it carries the ring", async () => {
  await onFakeClock(async () => {
    const view = renderHeader(true);

    try {
      const header = screen.getByTestId("artifact-header");
      expect(header.className).toContain(highlightRing);
      // Presence, not the value: the only reader is a presence selector
      // (`[data-landing-ring]`, `e2e/review-routes.ts`), and the two other trees that spell this
      // mark render it as `""` and as `"true"`. A row that pinned one of those would fail on the
      // other for nothing a reader or a wait can see.
      expect(header.hasAttribute("data-landing-ring")).toBe(true);

      // Two seconds of it, and no shorter.
      await tick(1_900);
      expect(header.className).toContain(highlightRing);
      expect(header.hasAttribute("data-landing-ring")).toBe(true);

      // And gone at the end of them, the ring and the mark together.
      await tick(200);
      expect(header.className).not.toContain(highlightRing);
      expect(header.hasAttribute("data-landing-ring")).toBe(false);
    } finally {
      view.unmount();
    }
  });
});

// A page reached by any other route draws the header with no highlight at all, so the mark is
// absent from its first frame: the review layers' wait for it is satisfied on every such page
// without waiting.
test("a header with no landing highlight carries neither the ring nor the mark", () => {
  const view = renderHeader(false);

  try {
    const header = screen.getByTestId("artifact-header");
    expect(header.className).not.toContain(highlightRing);
    expect(header.hasAttribute("data-landing-ring")).toBe(false);
  } finally {
    view.unmount();
  }
});
