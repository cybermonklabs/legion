import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { Artifact } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { VersionControl } from "./VersionControl";

const spec = {
  approval: { latest_version: 1, state: "requested" },
  created_at: "2026-11-01T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-spec",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  ref_key: "CORE-1",
  session_id: null,
  slug: "spec-md",
  versions: [],
} as unknown as Artifact;

function renderControl(copied: boolean) {
  const toolbar: DocumentToolbar = {
    connection: "connected",
    copyBlockLink: async () => copied,
    isNamingVersion: false,
    pending: undefined,
    requestNamedVersion: () => {},
    versions: [],
  };
  return render(
    <VersionControl
      artifact={spec}
      isClosed={false}
      onShowDiffChange={() => {}}
      onVersionChange={() => {}}
      showDiff={false}
      toolbar={toolbar}
      version={undefined}
    />
  );
}

test("the version control reports whether copying a document link succeeded", async () => {
  const user = userEvent.setup();
  for (const [copied, message] of [
    [true, "Copied"],
    [false, "Copy failed - select the text"],
  ] as const) {
    const view = renderControl(copied);
    try {
      await user.click(screen.getByRole("button", { name: "Copy link to block" }));
      expect((await screen.findByText(message)).textContent).toBe(message);
    } finally {
      view.unmount();
    }
  }
});

// No approval beside the filters: the approval entries stay in the identity block
// (dispatch://INFRA-1196 R31).
test("the version control draws no approval", () => {
  const view = renderControl(true);
  try {
    expect(screen.queryByRole("button", { name: /^Approve/ })).toBeNull();
    expect(view.container.textContent ?? "").not.toMatch(/approv/i);
  } finally {
    view.unmount();
  }
});
