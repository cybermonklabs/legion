import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { RefPreviewHost } from "./RefPreview";
import { closeRefPreview, referenceTriggerProps } from "./ref-preview-state";

// The hover card's host is loaded beside the route rather than in the entry chunk
// (`features/shell/deferred-shell.ts`), so a page draws its references before the host's
// document listeners exist. The event that put the reader on a reference - a `focusin`, or the
// `pointerover` of a pointer now resting there - fired before those listeners, and no second
// one is coming: the card never opened at all. The host reads both where the browser keeps
// them, and this row is the focus half. The pointer half is read from the hover chain
// (`:hover`), which happy-dom does not keep - no pointer moves in this process - so what proves
// it is the browser row `e2e/heavy-bodies.e2e.ts`'s hover card of a comment of 8,000 list
// items, the page whose own long body holds this chunk up longest.

afterEach(() => {
  closeRefPreview();
});

function Page({ host }: { host: boolean }): ReactNode {
  return (
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <a href="/issues/CORE-1" {...referenceTriggerProps({ key: "CORE-1", kind: "issue" })}>
          the reference
        </a>
        {host ? <RefPreviewHost /> : null}
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a reference the reader focused before the card's host arrived opens its card", () => {
  // The card's own read is never answered here: what this row is about is the card opening, and
  // until its target answers the card heads with the reference's short form.
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => new Promise(() => {}));
  const view = render(<Page host={false} />);
  try {
    act(() => {
      screen.getByRole("link", { name: "the reference" }).focus();
    });
    expect(screen.queryByRole("tooltip")).toBeNull();

    view.rerender(<Page host />);

    expect(screen.getByRole("tooltip").textContent).toContain("CORE-1");
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});
