import { beforeEach, expect, test } from "bun:test";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { type FakeDocumentRuntime, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import type { EditorHandle } from "../doc/editor";
import { DocumentRuntime, type DocumentRuntimeValue } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import {
  DocumentAdmissionProvider,
  type DocumentAdmissions,
  useDocumentAdmissions,
} from "./document-admission";
import { StagedDocument } from "./StagedDocument";

// What the editor of the document a row is edited in tells the page of its room's admission
// (`document-admission.tsx`), which the outline's rows read: no row of a document its room
// admitted read-only draws the cue that it takes typing. The page's own `ProofDocument` stands
// here, with a fake room whose answers each test gives (`fakeDocumentRuntime`): as the real
// room does, it admits the socket before it syncs, and the editor is made only once it has.

answerTextReadsWithTheSeededText();

beforeEach(() => {
  answerRequests(...documentSurfaceHandlers());
});

/** What the page holds of each document's admission, as the outline's rows read it. */
function AdmissionsProbe({ seen }: { seen: { admissions?: DocumentAdmissions } }): ReactNode {
  seen.admissions = useDocumentAdmissions();
  return null;
}

/** What the page's editor of `artifact` is drawn with: drawn or not, on an issue closed or open,
 *  and the runtime its connections are opened by. */
interface Drawn {
  readonly drawn?: boolean;
  readonly isClosed?: boolean;
  readonly runtime?: DocumentRuntimeValue;
}

/** The page's editor of `artifact`, its room `fake` unless `runtime` names another, under the
 *  page's admissions, drawn while `drawn` on an issue closed while `isClosed`. It hands back what
 *  the page holds of the document's admission and every editor the document handed the row. */
function renderStaged(fake: FakeDocumentRuntime) {
  const client = createQueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const seen: { admissions?: DocumentAdmissions } = {};
  const handed: (EditorHandle | undefined)[] = [];
  const draw = ({ drawn = true, isClosed = false, runtime = fake.runtime }: Drawn) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={runtime}>
          <MarginProvider>
            <DocumentAdmissionProvider>
              <AdmissionsProbe seen={seen} />
              {drawn ? (
                <StagedDocument
                  artifact={artifact}
                  isClosed={isClosed}
                  issueKey="CORE-1"
                  onEditorHandle={(handle) => handed.push(handle)}
                  onToolbarChange={() => {}}
                  onVersionChange={() => {}}
                  showDiff={false}
                />
              ) : null}
            </DocumentAdmissionProvider>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw({}));
  return {
    admission: () => seen.admissions?.get(artifact.id),
    handed,
    redraw: (next: Drawn) => view.rerender(draw(next)),
    view,
  };
}

/** The room of the newest connection the page opened answers its socket: it admits it, read-only
 *  or not, and then syncs, so the page makes that connection's editor. */
async function admitAndSync(fake: FakeDocumentRuntime, count: number, readOnly: boolean) {
  await waitFor(() => expect(fake.connections).toHaveLength(count));
  const room = fake.callbacks.at(-1);
  await act(async () => room?.onAdmission(readOnly));
  await act(async () => room?.onSynced());
}

// The room's read-only answer is the page's as soon as the article says so, before the editor is
// made: the rows of that document drop the cue while the row being edited waits for its editor.
test("an open issue's editor tells the page its room admitted the document read-only, and each later admission", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const staged = renderStaged(fake);
  try {
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    await act(async () => fake.callbacks.at(-1)?.onAdmission(true));
    await waitFor(() =>
      expect(
        staged.admission(),
        "the room's read-only admission of an open issue's document did not reach the page"
      ).toBe(true)
    );
    expect(staged.handed, "the editor was made before its room's socket synced").toEqual([]);
    await act(async () => fake.callbacks.at(-1)?.onSynced());
    await waitFor(() => expect(staged.handed.at(-1), "no editor was handed over").toBeDefined());
    expect(staged.admission(), "the editor's making took the read-only admission off").toBe(true);

    // The socket reconnects and the room admits it to write, then read-only again.
    await act(async () => fake.admit(false));
    await waitFor(() =>
      expect(staged.admission(), "a later admission to write did not reach the page").toBe(false)
    );
    await act(async () => fake.admit(true));
    await waitFor(() =>
      expect(staged.admission(), "a later read-only admission did not reach the page").toBe(true)
    );
  } finally {
    staged.view.unmount();
  }
});

// The article says the document takes typing from the moment it is drawn, as `ProofDocument`
// starts every connection, and no room has answered then: that is no admission, and it does not
// replace the one the page holds from an editor of the document before.
test("a room's admission stands after its editor closes, until the next editor's room answers", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const staged = renderStaged(fake);
  try {
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    expect(
      staged.admission(),
      "the page held an admission for the document before its room answered"
    ).toBeUndefined();
    await admitAndSync(fake, 1, true);
    await waitFor(() =>
      expect(staged.admission(), "the room's read-only admission did not reach the page").toBe(true)
    );

    staged.redraw({ drawn: false });
    expect(staged.admission(), "the room's admission went with its editor").toBe(true);

    staged.redraw({ drawn: true });
    await waitFor(() => expect(fake.connections).toHaveLength(2));
    expect(
      staged.view.container.querySelector(".dispatch-doc"),
      "the next editor's article does not say the document takes typing before its room answers"
    ).toHaveAttribute("data-read-only", "false");
    expect(
      staged.admission(),
      "the next editor took the admission the page held off before its room answered"
    ).toBe(true);

    await admitAndSync(fake, 2, false);
    await waitFor(() =>
      expect(
        staged.admission(),
        "the next editor's room admitted the document to write, and the page still holds it read-only"
      ).toBe(false)
    );
  } finally {
    staged.view.unmount();
  }
});

// An editor that opens a new connection to its room - here as the runtime it connects by changes -
// tears its editor down and marks its article as taking typing again, as it starts every
// connection: the mark changes, but no room has answered, and the admission the page holds stands.
test("an editor's new connection leaves the page's admission as it was until its room answers", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const staged = renderStaged(fake);
  try {
    await admitAndSync(fake, 1, true);
    await waitFor(() =>
      expect(staged.admission(), "the room's read-only admission did not reach the page").toBe(true)
    );

    const next = fakeDocumentRuntime({ text: "The live document" });
    staged.redraw({ runtime: next.runtime });
    await waitFor(() => expect(next.connections).toHaveLength(1));
    expect(
      staged.view.container.querySelector(".dispatch-doc"),
      "the new connection's article does not say the document takes typing before its room answers"
    ).toHaveAttribute("data-read-only", "false");
    expect(
      staged.admission(),
      "the new connection took the admission the page held off before its room answered"
    ).toBe(true);

    await admitAndSync(next, 1, false);
    await waitFor(() =>
      expect(
        staged.admission(),
        "the new connection's room admitted the document to write, and the page still holds it read-only"
      ).toBe(false)
    );
  } finally {
    staged.view.unmount();
  }
});

// A closed issue's article says read-only for the close itself, and the server admits every socket
// of a closed issue read-only: none of it is the room's answer about the document, and held, it
// would keep the cue off the document's rows once the issue is reopened.
test("the editor tells the page no admission while the issue is closed", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const staged = renderStaged(fake);
  try {
    await admitAndSync(fake, 1, false);
    await waitFor(() =>
      expect(staged.admission(), "the room's admission to write did not reach the page").toBe(false)
    );

    staged.redraw({ isClosed: true });
    expect(
      staged.view.container.querySelector(".dispatch-doc"),
      "the closed issue's article does not say read-only"
    ).toHaveAttribute("data-read-only", "true");
    await act(async () => fake.admit(true));
    expect(staged.admission(), "the page held a closed issue's admission as its room's").toBe(
      false
    );

    // An editor opened on the closed issue, its room admitting it read-only for the close.
    staged.redraw({ drawn: false, isClosed: true });
    staged.redraw({ isClosed: true });
    await admitAndSync(fake, 2, true);
    await waitFor(() => expect(staged.handed.at(-1), "no editor was handed over").toBeDefined());
    expect(
      staged.admission(),
      "the page held the admission of an editor opened on a closed issue as its room's"
    ).toBe(false);
  } finally {
    staged.view.unmount();
  }
});
