import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ArtifactRoutePanel } from "./ArtifactRoutePanel";
import { EMPTY_STATE_LINES } from "./page-words";

/**
 * The artifact an `/artifacts/<slug>` address opens is a region named by what it holds
 * (dispatch://INFRA-1196 R3, R21): nothing on the page is named Artifacts, and it is no
 * `tabpanel`, since the issue page has no tablist for one to belong to. A slug the issue does not
 * hold draws the page's own line, never the slug (part N's R2).
 */
test("an opened artifact the issue does not hold draws the page's line, not its slug, and no tabpanel", () => {
  const view = render(
    <MemoryRouter>
      <ArtifactRoutePanel
        artifact={undefined}
        artifactRoute={{ key: "CORE-1", kind: "artifact", slug: "notes-md" }}
        isClosed={false}
      >
        {null}
      </ArtifactRoutePanel>
    </MemoryRouter>
  );
  try {
    expect(view.getByRole("region", { name: EMPTY_STATE_LINES.document_missing })).toBeTruthy();
    expect(view.getByText(EMPTY_STATE_LINES.document_missing)).toBeTruthy();
    expect(view.container.textContent).not.toContain("notes-md");
    expect(view.container.textContent).not.toMatch(/\bartifact/iu);
    expect(view.queryAllByRole("region", { name: "Artifacts" })).toHaveLength(0);
    expect(view.queryAllByRole("tabpanel")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});
