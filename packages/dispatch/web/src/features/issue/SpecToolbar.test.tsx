import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { tabTrailingClassName } from "../../components/tab-classes";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { SpecToolbar } from "./SpecToolbar";

function renderToolbar(copied: boolean) {
  const toolbar: DocumentToolbar = {
    connection: "connected",
    copyBlockLink: async () => copied,
    isNamingVersion: false,
    pending: undefined,
    requestNamedVersion: () => {},
    versions: [],
  };
  return render(
    <SpecToolbar
      isClosed={false}
      onShowDiffChange={() => {}}
      onVersionChange={() => {}}
      reference={{ key: "CORE-1", kind: "spec" }}
      showDiff={false}
      toolbar={toolbar}
      version={undefined}
    />
  );
}

test("SpecToolbar reports whether copying a document link succeeded", async () => {
  const user = userEvent.setup();
  for (const [copied, message] of [
    [true, "Copied"],
    [false, "Copy failed - select the text"],
  ] as const) {
    const view = renderToolbar(copied);
    try {
      await user.click(screen.getByRole("button", { name: "Copy link to block" }));
      expect((await screen.findByText(message)).textContent).toBe(message);
    } finally {
      view.unmount();
    }
  }
});

// The toolbar is what the strip trails, and the space the issue page keeps for that strip
// keeps a box of these same classes for it (`issue-page-skeleton.tsx`): a row of its own below
// `md`, at the end of the tab row from `md` up. Both read them from one place, so a change to
// the toolbar's own row cannot leave that space a row short (dispatch://INFRA-341).
test("SpecToolbar stands in the box the strip's space is kept at", () => {
  const view = renderToolbar(true);
  try {
    expect(view.container.firstElementChild?.className).toBe(tabTrailingClassName);
  } finally {
    view.unmount();
  }
});
