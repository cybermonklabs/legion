import { expect, test } from "bun:test";
import { render } from "@testing-library/react";

import { ArtifactRoutePanel } from "./ArtifactRoutePanel";

/**
 * The panel an `/artifacts` address opens is named by what it holds (dispatch://INFRA-1196
 * R21). It was a `tabpanel` named by `aria-labelledby="issue-artifacts-tab"`, an id nothing has
 * rendered since the issue page's tablist went, so it was announced with no name at all.
 */
test("the artifacts panel is a named panel and no tabpanel", () => {
  const view = render(
    <ArtifactRoutePanel
      active
      artifact={undefined}
      artifactRoute={undefined}
      isClosed={false}
      isPrimaryArtifactRoute={false}
      mounted={false}
      onShowDiffChange={() => {}}
      route={{ key: "CORE-1", kind: "issue" }}
      showDiff={false}
      toolbar={undefined}
    >
      {null}
    </ArtifactRoutePanel>
  );
  try {
    expect(view.getByRole("region", { name: "Artifacts" })).toBeTruthy();
    expect(view.queryAllByRole("tabpanel")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});
