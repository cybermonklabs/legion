import { beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { ApiError, api } from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import { ArtifactHeader } from "../artifacts/ArtifactHeader";
import { type DocumentToolbar, ProofDocument } from "../doc/ProofDocument";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { NoTooltips } from "./NoTooltips";
import { VersionControl } from "./VersionControl";

/**
 * The name dialog while a version's name is saved (`NameVersionDialog.tsx`), one case per row of
 * the plan's state table ST-6 (decisions 223, 245 and 362): inside the new issue page's
 * `NoTooltips` Save and the text box are busy and refused, never disabled, and the dialog closes
 * once the read of the document after the write is in, giving the focus back once to Name
 * version; on today's pages they are disabled and the dialog closes as the write lands. Each case
 * holds the write and that read back until its row's moment, and counts what was sent.
 */

answerTextReadsWithTheSeededText();

/** The document's own reads beside its record, which each case answers itself. */
beforeEach(() => {
  answerRequests(...documentSurfaceHandlers());
});

/** Where the dialog is drawn: inside the new issue page's `NoTooltips`, or on today's pages. */
type Surface = "inside" | "outside";

/**
 * A document's editor (`ProofDocument`, which draws the dialog and makes the version's write) with
 * the control that opens its dialog, as each page draws them: inside `NoTooltips`, the new issue
 * page's version control beside the views, which reads the editor's toolbar (`VersionControl`);
 * outside it, today's document header (`ArtifactHeader`). Each version the editor opens once it
 * is named goes into `opened`.
 */
function NamingPage({ opened, surface }: { opened: number[]; surface: Surface }): ReactNode {
  const [toolbar, setToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const editor = (
    <ProofDocument
      artifact={artifact}
      highlight={undefined}
      isClosed={false}
      onToolbarChange={setToolbar}
      onVersionChange={(version) => {
        if (version !== null) opened.push(version);
      }}
      owner={{ key: "CORE-1", kind: "issue" }}
      showDiff={false}
      surface="page"
      user={{ event_head: "0", kind: "user", login: "alice" }}
      version={undefined}
    />
  );
  if (surface === "outside") {
    return (
      <ArtifactHeader
        artifact={artifact}
        highlight={false}
        showVersionPicker={false}
        toolbar={toolbar}
        version={undefined}
      >
        {editor}
      </ArtifactHeader>
    );
  }
  return (
    <NoTooltips>
      {toolbar === undefined ? null : (
        <VersionControl
          artifact={artifact}
          isClosed={false}
          onShowDiffChange={() => {}}
          onVersionChange={() => {}}
          showDiff={false}
          toolbar={toolbar}
          version={undefined}
        />
      )}
      {editor}
    </NoTooltips>
  );
}

/** The version a name write makes. */
const named: Version = {
  authors: [{ id: "alice", kind: "user" }],
  created_at: "2026-09-10T00:00:00Z",
  named: true,
  number: 1,
  summary: "Settled the rollout",
};

/** A dialog opened from its control with a name typed into it, and what the case answers itself:
 *  the version's write and the document's read after it, each held until the case lets it go, and
 *  counted. */
interface Naming {
  readonly box: HTMLElement;
  readonly dialog: HTMLElement;
  /** Lets the held read of the document after the write answer `answer`. */
  readonly answerReread: (answer: Artifact) => Promise<void>;
  /** Lets the held write answer `answer`. */
  readonly landWrite: (answer: Version) => Promise<void>;
  /** Lets the held write refuse with `refusal`. */
  readonly refuseWrite: (refusal: Error) => Promise<void>;
  /** Every version the editor opened. */
  readonly opened: readonly number[];
  readonly opener: HTMLElement;
  /** The document's record reads: the load's, then the one after the write. */
  readonly reads: Mock<typeof api.getArtifact>;
  readonly save: HTMLElement;
  readonly user: UserEvent;
  /** The version writes sent. */
  readonly writes: Mock<typeof api.createArtifactVersion>;
  readonly done: () => void;
}

/** Draws `NamingPage` on `surface`, presses its Name version, and types a name into the dialog,
 *  with the caret left in the box. A focused control that turns disabled drops the focus there,
 *  as Chromium does (`emulateChromiumFocusFixup`). */
async function openNaming(surface: Surface): Promise<Naming> {
  const user = userEvent.setup();
  const write = Promise.withResolvers<Version>();
  const reread = Promise.withResolvers<Artifact>();
  const writes = spyOn(api, "createArtifactVersion").mockReturnValue(write.promise);
  const reads = spyOn(api, "getArtifact")
    .mockResolvedValueOnce(artifact)
    .mockReturnValueOnce(reread.promise);
  const unemulate = emulateChromiumFocusFixup();
  const opened: number[] = [];
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={createQueryClient()}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <NamingPage opened={opened} surface={surface} />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const done = () => {
    view.unmount();
    unemulate();
    writes.mockRestore();
    reads.mockRestore();
  };
  try {
    const opener = await screen.findByRole("button", { name: "Name version" });
    await user.click(opener);
    const dialog = await screen.findByRole("dialog", { name: "Name this version" });
    const box = within(dialog).getByLabelText("What changed?");
    await waitFor(() => expect(box).toHaveFocus());
    await user.keyboard("Settled the rollout");
    const save = within(dialog).getByRole("button", { name: "Save" });
    return {
      answerReread: async (answer) => {
        await act(async () => reread.resolve(answer));
      },
      box,
      dialog,
      done,
      landWrite: async (answer) => {
        await act(async () => write.resolve(answer));
      },
      opened,
      opener,
      reads,
      refuseWrite: async (refusal) => {
        await act(async () => write.reject(refusal));
      },
      save,
      user,
      writes,
    };
  } catch (error) {
    done();
    throw error;
  }
}

/** Whether `control` is marked busy and refused while staying focusable, the shape decision 223
 *  gives a control whose write is out inside `NoTooltips`. */
function expectBusyNotDisabled(control: HTMLElement): void {
  expect(control).toHaveAttribute("aria-busy", "true");
  expect(control).toHaveAttribute("aria-disabled", "true");
  expect(control).not.toBeDisabled();
}

// ST-6, inside `NoTooltips`: Save sends one write, and while the name is saved Save and the box
// are marked busy and refused, never disabled, so the focus stays on Save, which the press left
// it on, rather than falling to the page's body as a disabled control drops it in Chromium.
test("a version name being saved marks Save busy and never disabled", async () => {
  const naming = await openNaming("inside");
  try {
    await naming.user.click(naming.save);
    await waitFor(() => expect(naming.writes).toHaveBeenCalledTimes(1));
    expect(naming.writes.mock.calls[0]?.slice(0, 2)).toEqual([
      artifact.id,
      { summary: "Settled the rollout" },
    ]);
    await waitFor(() => expectBusyNotDisabled(naming.save));
    expectBusyNotDisabled(naming.box);
    expect(naming.save).toHaveFocus();
    expect(naming.writes).toHaveBeenCalledTimes(1);
  } finally {
    naming.done();
  }
});

// ST-6: while the write is out, Enter in the box and a press of Save send nothing more, and the
// focus stays where the reader left it.
test("Enter while the write is out sends nothing more", async () => {
  const naming = await openNaming("inside");
  try {
    await naming.user.keyboard("{Enter}");
    await waitFor(() => expectBusyNotDisabled(naming.box));
    await naming.user.keyboard("{Enter}");
    expect(naming.box).toHaveFocus();
    await naming.user.click(naming.save);
    expect(naming.save).toHaveFocus();
    expect(naming.writes).toHaveBeenCalledTimes(1);
    expect(naming.dialog).toBeInTheDocument();
  } finally {
    naming.done();
  }
});

// ST-6: the write has landed and the read of the document after it is still out, so the version
// is not yet on the list. The dialog stays open and busy: Enter and Save send nothing, and the
// focus stays in the box.
test("Enter after the write landed and before the re-read sends nothing and keeps the dialog open", async () => {
  const naming = await openNaming("inside");
  try {
    await naming.user.keyboard("{Enter}");
    await waitFor(() => expect(naming.writes).toHaveBeenCalledTimes(1));
    await naming.landWrite(named);
    await waitFor(() => expect(naming.reads).toHaveBeenCalledTimes(2));
    await naming.user.keyboard("{Enter}");
    expect(naming.box).toHaveFocus();
    await naming.user.click(naming.save);
    expect(naming.writes).toHaveBeenCalledTimes(1);
    expect(naming.dialog).toBeInTheDocument();
    expectBusyNotDisabled(naming.save);
    expectBusyNotDisabled(naming.box);
    expect(naming.opened).toEqual([]);
  } finally {
    naming.done();
  }
});

// ST-6: once the read after the write is in, the versions list the one just made, the dialog
// closes, and the focus goes to the control that opened it, Name version, and nowhere before it.
test("once the re-read is drawn the dialog closes and the focus goes once to Name version", async () => {
  const naming = await openNaming("inside");
  const moves: EventTarget[] = [];
  const noteMove = (event: FocusEvent) => {
    if (event.target !== null) moves.push(event.target);
  };
  try {
    await naming.user.keyboard("{Enter}");
    await waitFor(() => expect(naming.writes).toHaveBeenCalledTimes(1));
    await naming.landWrite(named);
    await waitFor(() => expect(naming.reads).toHaveBeenCalledTimes(2));
    expect(naming.dialog).toBeInTheDocument();
    document.addEventListener("focusin", noteMove, true);
    await naming.answerReread({ ...artifact, versions: [named] });
    await waitFor(() => expect(naming.dialog).not.toBeInTheDocument());
    expect(naming.opener).toHaveFocus();
    expect(moves).toEqual([naming.opener]);
    expect(
      await screen.findByRole("option", { name: "Version 1 — Settled the rollout" })
    ).toBeInTheDocument();
    expect(naming.opened).toEqual([1]);
    expect(naming.writes).toHaveBeenCalledTimes(1);
    expect(naming.opener).toHaveFocus();
  } finally {
    document.removeEventListener("focusin", noteMove, true);
    naming.done();
  }
});

// ST-6: a name Dispatch refuses keeps the dialog open with the refusal's line, takes the busy
// marks off, and leaves the caret in the box, ready for another press. Nothing is read again.
test("a refused name keeps the dialog open with its line and the focus in the box", async () => {
  const naming = await openNaming("inside");
  try {
    await naming.user.keyboard("{Enter}");
    await waitFor(() => expectBusyNotDisabled(naming.box));
    await naming.refuseWrite(
      new ApiError(422, { code: "INVALID_VERSION", error: "The version cannot be named." })
    );
    expect(await within(naming.dialog).findByRole("alert")).toHaveTextContent(
      "Could not name this version. Try again."
    );
    for (const control of [naming.save, naming.box]) {
      expect(control).not.toHaveAttribute("aria-busy");
      expect(control).not.toHaveAttribute("aria-disabled");
      expect(control).not.toBeDisabled();
    }
    expect(naming.box).toHaveFocus();
    expect(naming.dialog).toBeInTheDocument();
    expect(naming.writes).toHaveBeenCalledTimes(1);
    expect(naming.reads).toHaveBeenCalledTimes(1);
  } finally {
    naming.done();
  }
});

// ST-6, today's pages: outside `NoTooltips` Save and the box are `disabled` while the name is
// saved, and the dialog closes as the write lands, with no read of the document after it.
test("with tooltips on, Save and the name are disabled while saving, as before", async () => {
  const naming = await openNaming("outside");
  try {
    await naming.user.click(naming.save);
    await waitFor(() => expect(naming.save).toBeDisabled());
    expect(naming.box).toBeDisabled();
    expect(naming.save).not.toHaveAttribute("aria-busy");
    expect(naming.box).not.toHaveAttribute("aria-disabled");
    await naming.landWrite(named);
    await waitFor(() => expect(naming.dialog).not.toBeInTheDocument());
    expect(naming.opened).toEqual([1]);
    expect(naming.writes).toHaveBeenCalledTimes(1);
    expect(naming.reads).toHaveBeenCalledTimes(1);
  } finally {
    naming.done();
  }
});
