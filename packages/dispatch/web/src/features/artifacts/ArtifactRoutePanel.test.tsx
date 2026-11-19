import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ArtifactRoutePanel } from "./ArtifactRoutePanel";

/**
 * The artifact an `/artifacts/<slug>` address opens is a region named by what it holds
 * (dispatch://INFRA-1196 R3, R21): nothing on the page is named Artifacts, and it is no
 * `tabpanel`, since the issue page has no tablist for one to belong to.
 */
test("an opened artifact the issue does not hold is named by its slug, and no tabpanel", () => {
  const view = render(
    <MemoryRouter>
      <ArtifactRoutePanel
        artifact={undefined}
        artifactRoute={{ key: "CORE-1", kind: "artifact", slug: "notes-md" }}
        isClosed={false}
        toolbar={undefined}
      >
        {null}
      </ArtifactRoutePanel>
    </MemoryRouter>
  );
  try {
    expect(view.getByRole("region", { name: "notes-md" })).toBeTruthy();
    expect(view.queryAllByRole("region", { name: "Artifacts" })).toHaveLength(0);
    expect(view.queryAllByRole("tabpanel")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});
