import { expect, test } from "bun:test";
import { render } from "@testing-library/react";

import { RoleDocumentPanel } from "./RoleDocumentPanel";

/**
 * The two role-document panels are named by what they hold (dispatch://INFRA-1196 R21). They
 * were `tabpanel`s named by `aria-labelledby="issue-<tab>-tab"`, an id nothing has rendered
 * since the tablist went, so each was announced with no name at all.
 */

const props = {
  activated: false,
  artifact: undefined,
  isClosed: false,
  issueKey: "CORE-1",
  onToolbarChange: () => () => {},
  onVersionChange: () => {},
  showDiff: new Set<string>(),
  stage: () => {},
};

test("each role-document panel is a named panel and no tabpanel", () => {
  const view = render(
    <>
      <RoleDocumentPanel {...props} active tab="verification" />
      <RoleDocumentPanel {...props} active={false} tab="plan" />
    </>
  );
  try {
    expect(view.getByRole("region", { name: "Checks document" })).toBeTruthy();
    expect(view.queryAllByRole("tabpanel")).toHaveLength(0);
    // Hidden while another preset is open, named all the same.
    expect(view.container.querySelector("#issue-plan-panel")?.getAttribute("aria-label")).toBe(
      "Plan document"
    );
  } finally {
    view.unmount();
  }
});
