import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useEffect } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { issueQuery } from "../../api/queries";
import type { Artifact, IssueDetails, Version } from "../../api/types";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { parseIssuePath } from "../refs/routes";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { artifact, issueDetails } from "./decisions-filter/decisions-fixtures";
import { NoTooltips } from "./NoTooltips";
import type { SlotProps } from "./slot-props";
import {
  DocumentToolbarsProvider,
  useDocumentToolbars,
  useToolbarSink,
} from "./useDocumentToolbars";
import { VersionControl } from "./VersionControl";
import { useViewRowTools } from "./view-row-tools";

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

/** The issue's own read, as the page holds it while the control is drawn: the read Name version
 *  waits for once its write lands. */
function IssueRead(): ReactNode {
  useQuery(issueQuery("CORE-1"));
  return null;
}

// ST-6 for the control's own naming, with no editor of the document on the page: inside
// `NoTooltips` the dialog stays open and busy after the write lands, so Enter sends nothing more,
// until the issue's read, whose documents' versions the picker lists, is in again. Then it closes
// and the focus goes once to Name version, which stayed focusable meanwhile: refused with
// `aria-disabled`, never `disabled`, which would leave the dialog's close nothing to give it to.
test("with no editor on the page, Name version's dialog waits for the issue's read and gives the focus back once", async () => {
  const user = userEvent.setup();
  const unemulate = emulateChromiumFocusFixup();
  const write = Promise.withResolvers<Version>();
  const reread = Promise.withResolvers<IssueDetails>();
  const createArtifactVersion = spyOn(api, "createArtifactVersion").mockReturnValue(write.promise);
  const getIssue = spyOn(api, "getIssue")
    .mockResolvedValueOnce(issueDetails())
    .mockReturnValueOnce(reread.promise);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <NoTooltips>
        <IssueRead />
        <VersionControl
          artifact={spec}
          isClosed={false}
          onShowDiffChange={() => {}}
          onVersionChange={() => {}}
          showDiff={false}
          toolbar={undefined}
          version={undefined}
        />
      </NoTooltips>
    </QueryClientProvider>
  );
  const moves: EventTarget[] = [];
  const noteMove = (event: FocusEvent) => {
    if (event.target !== null) moves.push(event.target);
  };
  try {
    await waitFor(() => expect(client.getQueryState(["issue", "CORE-1"])?.status).toBe("success"));
    const opener = screen.getByRole("button", { name: "Name version" });
    await user.click(opener);
    const dialog = await screen.findByRole("dialog", { name: "Name this version" });
    const box = within(dialog).getByLabelText("What changed?");
    await waitFor(() => expect(box).toHaveFocus());
    await user.keyboard("Settled the rollout{Enter}");
    await waitFor(() => expect(createArtifactVersion).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(opener).toHaveAttribute("aria-disabled", "true"));
    expect(opener).not.toBeDisabled();

    await act(async () =>
      write.resolve({
        authors: [{ id: "alice", kind: "user" }],
        created_at: "2026-11-02T00:00:00Z",
        named: true,
        number: 1,
        summary: "Settled the rollout",
      })
    );
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(2));
    await user.keyboard("{Enter}");
    expect(createArtifactVersion).toHaveBeenCalledTimes(1);
    expect(dialog).toBeInTheDocument();
    expect(box).toHaveFocus();
    expect(box).toHaveAttribute("aria-busy", "true");
    expect(box).not.toBeDisabled();

    document.addEventListener("focusin", noteMove, true);
    await act(async () => reread.resolve(issueDetails()));
    await waitFor(() => expect(dialog).not.toBeInTheDocument());
    expect(opener).toHaveFocus();
    expect(moves).toEqual([opener]);
    expect(createArtifactVersion).toHaveBeenCalledTimes(1);
  } finally {
    document.removeEventListener("focusin", noteMove, true);
    view.unmount();
    unemulate();
    getIssue.mockRestore();
    createArtifactVersion.mockRestore();
  }
});

/** Version `number` of a document, named `summary` where one is given. */
function version(number: number, summary?: string): Version {
  return {
    authors: [{ id: "alice", kind: "user" }],
    created_at: `2026-11-0${number}T00:00:00Z`,
    named: summary !== undefined,
    number,
    summary: summary ?? null,
  };
}

/** The issue's spec, with an approval requested of it, and the documents of the checks' and the
 *  plan's roles, and a document of no role, each with versions of its own. */
const SPEC = {
  ...artifact("artifact-spec", "spec", true),
  approval: { latest_version: 2, state: "requested" },
  versions: [version(1), version(2, "Settled the scope")],
} as unknown as Artifact;
const CHECKS = { ...artifact("artifact-checks", "verification-md"), versions: [version(1)] };
const PLAN = {
  ...artifact("artifact-plan", "plan-md"),
  versions: [version(1), version(2), version(3, "Split the work")],
};
const NOTES = {
  ...artifact("artifact-notes", "notes-md"),
  versions: [version(1, "First notes"), version(2)],
};
const ISSUE: IssueDetails = {
  ...issueDetails(),
  artifacts: [SPEC, CHECKS, PLAN, NOTES],
  plan_artifact_id: PLAN.id,
  verification_artifact_id: CHECKS.id,
};

/** An editor's controls, as an editor on the page reports them: connected, its versions the
 *  ones it has read, which can be newer than the issue's own read. */
function editorToolbar(versions: Version[], copied = true): DocumentToolbar {
  return {
    connection: "connected",
    copyBlockLink: async () => copied,
    isNamingVersion: false,
    pending: undefined,
    requestNamedVersion: () => {},
    versions,
  };
}

/** An editor of the document `artifactId` open on the page: it reports its controls through
 *  the page's one sink, as every editor on the page does (`useDocumentToolbars.ts`), the opened
 *  document's (`stage-landing.tsx`) and a row's alike. */
function OpenEditor({
  artifactId,
  toolbar,
}: {
  artifactId: string;
  toolbar: DocumentToolbar;
}): null {
  const sink = useToolbarSink();
  useEffect(() => {
    const report = sink(artifactId);
    report(toolbar);
    return () => report(undefined);
  }, [artifactId, sink, toolbar]);
  return null;
}

/** The version control the view row draws (`view-row-tools.tsx`) for the slot's props. */
function RowTools(props: SlotProps): ReactNode {
  return useViewRowTools(props).toolbar ?? null;
}

/** The row's version control at the address the router holds, the route read from it at every
 *  render, as the page's root reads it. */
function AddressedTools({ issue }: { issue: IssueDetails }): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return <RowTools issue={issue} route={route} />;
}

/** Where the router is, the outline its state hands an opened document, and which documents
 *  draw their changes against the current text: the page's toolbars' `diffs`, from which the
 *  stage draws an opened document's change view (`stage-landing.tsx`'s `showDiff`). */
function Where(): ReactNode {
  const { pathname, search, state } = useLocation();
  const { diffs } = useDocumentToolbars();
  const outline =
    typeof state === "object" && state !== null && "outline" in state ? String(state.outline) : "";
  return (
    <output data-diffs={[...diffs].join(" ")} data-outline={outline} data-testid="where">
      {`${pathname}${search}`}
    </output>
  );
}

/** The view row's version control at `address`, under the page's toolbars and inside
 *  `NoTooltips`, as the page draws it, with `editor` open beside it where one is given. */
function drawRowTools(
  address: string,
  editor?: { artifactId: string; toolbar: DocumentToolbar }
): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={[address]}>
      <QueryClientProvider client={client}>
        <NoTooltips>
          <DocumentToolbarsProvider>
            {editor === undefined ? null : <OpenEditor {...editor} />}
            <AddressedTools issue={ISSUE} />
            <Where />
          </DocumentToolbarsProvider>
        </NoTooltips>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// R3: the control reads a document only where the page shows one, the view's or the opened
// one's. The outline, the Decisions view, which has no document of its role, and the node menu
// open over the outline draw none, even with an editor of the spec open in the outline.
test("with no view on and no document open the version control is not drawn", async () => {
  for (const address of [
    "/issues/CORE-1",
    "/issues/CORE-1/decisions",
    "/issues/CORE-1/artifacts",
  ]) {
    const view = drawRowTools(address, {
      artifactId: SPEC.id,
      toolbar: editorToolbar([version(1)]),
    });
    try {
      await screen.findByTestId("where");
      expect(screen.queryByTestId("issue-version-control"), address).toBeNull();
      expect(screen.queryByRole("combobox", { name: "Version" }), address).toBeNull();
    } finally {
      view.unmount();
    }
  }
  // The same row under the Spec view draws it.
  const view = drawRowTools("/issues/CORE-1/spec");
  try {
    expect(await screen.findByTestId("issue-version-control")).toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// R3: under a view the versions are those of that view's document on today's model, and with a
// document open the document's; where the document's editor is on the page they are the ones it
// read, which a version just named makes newer than the issue's own read.
test("under the Spec view it lists the spec's versions, and under a document the document's", async () => {
  const notesEditor = {
    artifactId: NOTES.id,
    toolbar: editorToolbar([
      version(3, "Named in the editor"),
      version(2),
      version(1, "First notes"),
    ]),
  };
  const cases: readonly [string, typeof notesEditor | undefined, readonly string[]][] = [
    [
      "/issues/CORE-1/spec",
      undefined,
      ["Current version", "Version 2 — Settled the scope", "Version 1"],
    ],
    ["/issues/CORE-1/verification", undefined, ["Current version", "Version 1"]],
    [
      "/issues/CORE-1/plan",
      undefined,
      ["Current version", "Version 3 — Split the work", "Version 2", "Version 1"],
    ],
    [
      "/issues/CORE-1/artifacts/notes-md",
      undefined,
      ["Current version", "Version 2", "Version 1 — First notes"],
    ],
    [
      "/issues/CORE-1/artifacts/spec",
      undefined,
      ["Current version", "Version 2 — Settled the scope", "Version 1"],
    ],
    [
      "/issues/CORE-1/artifacts/notes-md",
      notesEditor,
      [
        "Current version",
        "Version 3 — Named in the editor",
        "Version 2",
        "Version 1 — First notes",
      ],
    ],
  ];
  for (const [address, editor, options] of cases) {
    const view = drawRowTools(address, editor);
    try {
      const control = await screen.findByTestId("issue-version-control");
      await waitFor(() =>
        expect(
          within(control)
            .getAllByRole("option")
            .map((option) => option.textContent),
          `${address} with ${editor === undefined ? "no" : "its"} editor`
        ).toEqual([...options])
      );
    } finally {
      view.unmount();
    }
  }
});

// R3: the control names a version, and says so: "Name version" opens the name dialog where no
// editor of the document is on the page, and asks the editor to name one where it is. No
// control of it, nor of the dialog, reads "Name" alone.
test("Name version names a version, and no control reads Name alone", async () => {
  const user = userEvent.setup();
  const alone = drawRowTools("/issues/CORE-1/spec");
  try {
    const control = await screen.findByTestId("issue-version-control");
    await user.click(within(control).getByRole("button", { name: "Name version" }));
    const dialog = await screen.findByRole("dialog", { name: "Name this version" });
    expect(within(dialog).getByLabelText("What changed?")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Name" })).toBeNull();
  } finally {
    alone.unmount();
  }

  const toolbar = editorToolbar([version(1)]);
  const requestNamedVersion = spyOn(toolbar, "requestNamedVersion");
  const beside = drawRowTools("/issues/CORE-1/artifacts/notes-md", {
    artifactId: NOTES.id,
    toolbar,
  });
  try {
    const control = await screen.findByTestId("issue-version-control");
    await waitFor(() => expect(within(control).getByRole("status")).toBeInTheDocument());
    await user.click(within(control).getByRole("button", { name: "Name version" }));
    expect(requestNamedVersion).toHaveBeenCalledTimes(1);
    // The editor's own dialog names it, so the control opens none of its own.
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("button", { name: "Name" })).toBeNull();
  } finally {
    beside.unmount();
  }
});

// R3: a pinned version is compared with the document's current text. With the document open,
// the comparison turns on that document's change view, which the stage draws it with
// (`showDiff`), and the control then goes back to the version itself. Under a view there is no
// text of the document to draw it over, so the comparison opens the document at the pinned
// version with its change view on. With no version pinned there is nothing to compare.
test("Compare shows the change view against the current text", async () => {
  const user = userEvent.setup();
  const opened = drawRowTools("/issues/CORE-1/artifacts/notes-md?v=1", {
    artifactId: NOTES.id,
    toolbar: editorToolbar([version(2), version(1, "First notes")]),
  });
  try {
    const control = await screen.findByTestId("issue-version-control");
    const where = screen.getByTestId("where");
    expect(where).toHaveAttribute("data-diffs", "");
    await user.click(within(control).getByRole("button", { name: "Diff vs current" }));
    expect(where).toHaveAttribute("data-diffs", NOTES.id);
    expect(where).toHaveTextContent("/issues/CORE-1/artifacts/notes-md?v=1");
    await user.click(within(control).getByRole("button", { name: "Show version" }));
    expect(where).toHaveAttribute("data-diffs", "");
  } finally {
    opened.unmount();
  }

  const underView = drawRowTools("/issues/CORE-1/spec?v=1");
  try {
    const control = await screen.findByTestId("issue-version-control");
    await user.click(within(control).getByRole("button", { name: "Diff vs current" }));
    const where = screen.getByTestId("where");
    await waitFor(() => expect(where).toHaveTextContent("/issues/CORE-1/artifacts/spec?v=1"));
    expect(where).toHaveAttribute("data-diffs", SPEC.id);
    // The spec opened at that version, its change view on, goes back to the outline.
    expect(where).toHaveAttribute("data-outline", "/issues/CORE-1");
    expect(
      within(screen.getByTestId("issue-version-control")).getByRole("button", {
        name: "Show version",
      })
    ).toBeInTheDocument();
  } finally {
    underView.unmount();
  }

  const current = drawRowTools("/issues/CORE-1/spec");
  try {
    const control = await screen.findByTestId("issue-version-control");
    expect(within(control).queryByRole("button", { name: "Diff vs current" })).toBeNull();
  } finally {
    current.unmount();
  }
});

// R3: the control copies a link to the document, at the version it reads, and says whether the
// copy succeeded: "Copied", or, where neither clipboard path takes it, the link to select by
// hand.
test("Copy link says the link was copied, or that the copy failed", async () => {
  const user = userEvent.setup();
  const clipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
  const execCommand = Object.getOwnPropertyDescriptor(document, "execCommand");
  const writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const view = drawRowTools("/issues/CORE-1/spec?v=2");
  try {
    const control = await screen.findByTestId("issue-version-control");
    const copy = within(control).getByRole("button", { name: /^Copy reference / });
    await user.click(copy);
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const link = writeText.mock.calls[0]?.[0] ?? "";
    expect(link).toMatch(/^dispatch:\/\/CORE-1\b.*2/);
    expect(await within(control).findByText("Copied", { exact: true })).toBeInTheDocument();

    Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
    Object.defineProperty(document, "execCommand", { configurable: true, value: () => false });
    await user.click(copy);
    const failed = await within(control).findByText(/^Copy failed -/);
    expect(failed).toHaveTextContent(`Copy failed - ${link}`);
    expect(within(control).queryByText("Copied", { exact: true })).toBeNull();
  } finally {
    view.unmount();
    if (clipboard === undefined) Reflect.deleteProperty(navigator, "clipboard");
    else Object.defineProperty(navigator, "clipboard", clipboard);
    if (execCommand === undefined) Reflect.deleteProperty(document, "execCommand");
    else Object.defineProperty(document, "execCommand", execCommand);
  }
});

// R3: the live connection and the link to the block the caret is in are an editor's, so they
// stand beside the editor of the document the control reads, read through the page's toolbars,
// and nowhere else: not with no editor on the page, and not beside another document's editor.
test("the connection dot and the caret's block link stand only beside an open editor", async () => {
  const cases: readonly [string, string | undefined, boolean][] = [
    ["/issues/CORE-1/spec", undefined, false],
    ["/issues/CORE-1/artifacts/notes-md", undefined, false],
    ["/issues/CORE-1/artifacts/notes-md", SPEC.id, false],
    ["/issues/CORE-1/artifacts/notes-md", NOTES.id, true],
    ["/issues/CORE-1/spec", SPEC.id, true],
  ];
  for (const [address, editorOf, beside] of cases) {
    const what = `${address} with ${editorOf ?? "no"} editor`;
    const view = drawRowTools(
      address,
      editorOf === undefined
        ? undefined
        : { artifactId: editorOf, toolbar: editorToolbar([version(1)]) }
    );
    try {
      const control = await screen.findByTestId("issue-version-control");
      if (beside) {
        expect(
          await within(control).findByRole("status", { name: "connected" }),
          what
        ).toBeVisible();
        expect(
          within(control).getByRole("button", { name: "Copy link to block" }),
          what
        ).toBeVisible();
      } else {
        expect(within(control).queryByRole("status"), what).toBeNull();
        expect(
          within(control).queryByRole("button", { name: "Copy link to block" }),
          what
        ).toBeNull();
        // What it draws instead is the issue's own read of the document's versions.
        expect(within(control).getByRole("button", { name: "Name version" }), what).toBeVisible();
      }
    } finally {
      view.unmount();
    }
  }
});

// R3, R31: the approval entries stay in the identity block. The spec waits on an approval, and
// the control draws none of it under the Spec view, with the spec open, or beside its editor.
test("no approval control is drawn", async () => {
  for (const [address, editor] of [
    ["/issues/CORE-1/spec", false],
    ["/issues/CORE-1/spec?v=2", false],
    ["/issues/CORE-1/artifacts/spec", true],
  ] as const) {
    const view = drawRowTools(
      address,
      editor ? { artifactId: SPEC.id, toolbar: editorToolbar([version(2), version(1)]) } : undefined
    );
    try {
      const control = await screen.findByTestId("issue-version-control");
      if (editor) await within(control).findByRole("status");
      expect(within(control).queryByRole("button", { name: /approv/i }), address).toBeNull();
      expect(control.textContent ?? "", address).not.toMatch(/approv/i);
    } finally {
      view.unmount();
    }
  }
});
