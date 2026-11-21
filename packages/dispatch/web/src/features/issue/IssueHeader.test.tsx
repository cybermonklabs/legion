import { expect, type Mock, spyOn, test } from "bun:test";
import { hashKey, QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the presses that end a title edit go inside one act, so React commits what they change after them as a browser does; user-event commits inside each event
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { settle } from "../../__tests__/fake-clock";
import { afterRescue, RescuedMain } from "../../__tests__/focus-rescue";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { api, type WriteBounds } from "../../api/client";
import { issueQuery, userStateQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  Artifact,
  ArtifactReview,
  Issue,
  IssueDetails,
  UpdateIssueInput,
  UserIssueState,
  UserState,
} from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { setActionKeys } from "../shell/keymap-keys";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { idlePriorityWrite, mockHeaderReads } from "./header-fixture";
import { IssueHeader } from "./IssueHeader";
import { stateForIssue } from "./pins";

// The issue title's editor, read with the real header and the page's focus rescue mounted around
// it as the app shell mounts it (`RescuedMain`).

const base: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-09-24T05:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  status: "in_progress",
  title: "Review the spec",
  updated_at: "2026-09-24T06:00:00Z",
  verification_artifact_id: "",
};

const details: IssueDetails = {
  ...base,
  artifacts: [],
  children: [],
  design_gates: [],
  holder_state: null,
  last_seen: null,
  open_asks: [],
  progress: { tasks: null, children: null },
  referenced_by_count: 0,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  takeable_at: null,
};

/** The text of the page outside the header, which a reader can click on. */
const PAGE_TEXT = "The issue's description";

type Patches = Mock<(key: string, input: UpdateIssueInput, bounds: WriteBounds) => Promise<Issue>>;

/** The header of the issue the cache holds, drawn again from each answer a write of it merges
 *  there, as `IssuePage` draws it. */
function CachedHeader(): ReactNode {
  const issue = useQuery({ ...issueQuery(base.key), staleTime: Infinity }).data;
  if (issue === undefined) return null;
  return (
    <IssueHeader
      documentArtifact={undefined}
      isClosed={false}
      issue={issue}
      priorityWrite={idlePriorityWrite}
      state={stateForIssue(undefined, issue.key)}
    />
  );
}

/** The header of `details` in the main content, the page's focus rescue mounted on it, with the
 *  page's text after it, and the title writes it makes answered as Dispatch answers them. */
function header(): { patches: Patches; unmount: () => void; user: UserEvent } {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(issueQuery(base.key).queryKey, details);
  // The header reads the project's repository mappings, its architecture sources and the issue's
  // subscribers at once; none of them is what these rows are about.
  const restoreReads = mockHeaderReads();
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const patches = answerIssuePatches().mockImplementation(async (_key, input) => ({
    ...base,
    title: input.title ?? base.title,
    updated_at: "2026-09-24T07:00:00Z",
  }));
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>
          <RescuedMain>
            <CachedHeader />
            <p>{PAGE_TEXT}</p>
          </RescuedMain>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    patches,
    unmount: () => {
      view.unmount();
      restoreReads();
      subscribers.mockRestore();
    },
    user,
  };
}

/**
 * Sends `press` as a browser has React commit it. A browser commits the update an event makes in
 * a microtask after that event, so the page's focus rescue, whose `focusout` listener on the
 * document runs before React's own, reads the page in a microtask of its own first; user-event
 * commits inside each event it sends, which no browser does. So these presses go out with
 * fireEvent inside one act, which commits what they changed once they have run.
 */
async function pressAsBrowser(press: () => void): Promise<void> {
  await act(async () => {
    press();
  });
}

/** Opens the title's editor with a click on its heading and writes `title` into it. */
async function editTitle(user: UserEvent, title: string): Promise<HTMLElement> {
  await user.click(await screen.findByRole("heading", { level: 1, name: base.title }));
  const editor = await screen.findByRole("textbox", { name: "Issue title" });
  await user.clear(editor);
  await user.keyboard(title);
  return editor;
}

/** The editor closed on the saved `title`, and the reader's next key, `t`, edits no title: no
 *  text box takes it, the heading still reads `title`, and the one write is the save's. */
async function expectNextKeyEditsNoTitle(
  user: UserEvent,
  patches: Patches,
  title: string
): Promise<void> {
  await waitFor(() => expect(patches).toHaveBeenCalledWith("CORE-1", { title }, keyedAttempt));
  expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument();
  expect(screen.getByRole("heading", { level: 1, name: title })).toBeInTheDocument();
  await user.keyboard("t");
  await afterRescue();
  expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument();
  expect(screen.getByRole("heading", { level: 1, name: title })).toBeInTheDocument();
  expect(patches).toHaveBeenCalledTimes(1);
}

// The reader's Enter saves the title and closes its editor: the editor's own `blur()` is the
// reader leaving it, so nothing reopens it in the heading's place, and the next key they type
// goes into no title.
test("Enter in the title's editor saves it and closes it, and the next key edits no title", async () => {
  const { patches, unmount, user } = header();
  try {
    const editor = await editTitle(user, "Renamed");
    await pressAsBrowser(() => fireEvent.keyDown(editor, { key: "Enter" }));
    await afterRescue();
    await expectNextKeyEditsNoTitle(user, patches, "Renamed");
  } finally {
    unmount();
  }
});

// So does the reader's click on the page's text outside the editor: the press lands on no
// control, the engine moves the focus to the page, and the blur saves and closes the editor.
test("a click on the page's text saves the title and closes its editor, and the next key edits no title", async () => {
  const { patches, unmount, user } = header();
  try {
    const editor = await editTitle(user, "Renamed");
    const text = screen.getByText(PAGE_TEXT);
    await pressAsBrowser(() => {
      // The press goes out as a browser's, its events in one act that React commits after them
      // all (`pressAsBrowser`): user-event's click commits inside each event it sends.
      fireEvent.pointerDown(text);
      fireEvent.mouseDown(text);
      // The engine's answer to a press on no control, which happy-dom leaves to the page.
      editor.blur();
      fireEvent.pointerUp(text);
      fireEvent.mouseUp(text);
      fireEvent.click(text);
    });
    await afterRescue();
    await expectNextKeyEditsNoTitle(user, patches, "Renamed");
  } finally {
    unmount();
  }
});

// An Enter that ends or moves an input method's composition commits the composed text into the
// editor and is not the reader's save, nor is an Escape that cancels the composition their
// discard: the editor stays open on what they wrote, with nothing saved, and the plain Enter after
// them saves it. user-event has no form for a composing key, so each is fired with `isComposing`.
test("Enter and Escape that end an IME composition in the title's editor save nothing and keep it open", async () => {
  const { patches, unmount, user } = header();
  try {
    const editor = await editTitle(user, "Renamed");
    for (const key of ["Enter", "Escape"]) {
      // user-event has no form for this: it cannot mark a key as an IME composition.
      await pressAsBrowser(() => fireEvent.keyDown(editor, { isComposing: true, key }));
      await afterRescue();
      expect(screen.getByRole("textbox", { name: "Issue title" }), `${key} composing`).toBe(editor);
      expect(editor, `${key} composing`).toHaveFocus();
      expect(editor, `${key} composing`).toHaveValue("Renamed");
    }
    expect(patches).not.toHaveBeenCalled();

    // The plain Enter goes out as a browser's, in one act that React commits after it
    // (`pressAsBrowser`): user-event commits inside each event it sends.
    await pressAsBrowser(() => fireEvent.keyDown(editor, { key: "Enter" }));
    await afterRescue();
    await expectNextKeyEditsNoTitle(user, patches, "Renamed");
  } finally {
    unmount();
  }
});

// The heading opens its editor when the reader activates it, not when it takes the focus: an
// editor opening on focus changes the page on focus (WCAG 2.2, 3.2.1 On Focus).
test("focusing the title's heading does not open its editor", async () => {
  const { unmount } = header();
  try {
    const heading = await screen.findByRole("heading", { level: 1, name: base.title });
    act(() => heading.focus());
    await afterRescue();
    expect(heading).toHaveFocus();
    expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument();
  } finally {
    unmount();
  }
});

// A click on the heading opens its editor, and so does Enter on the focused heading; the editor
// takes the focus either way.
test("a click on the title's heading, and Enter on it, open its editor", async () => {
  const { unmount, user } = header();
  try {
    await user.click(await screen.findByRole("heading", { level: 1, name: base.title }));
    expect(await screen.findByRole("textbox", { name: "Issue title" })).toHaveFocus();
    await user.keyboard("{Escape}");
    await afterRescue();
    const heading = await screen.findByRole("heading", { level: 1, name: base.title });
    expect(screen.queryByRole("textbox", { name: "Issue title" })).not.toBeInTheDocument();

    act(() => heading.focus());
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("textbox", { name: "Issue title" })).toHaveFocus();
  } finally {
    unmount();
  }
});

// The header's pin, read against the viewer's state as Dispatch holds it before and after the pin.

/** The issue the pin rows' header draws: unclaimed, at the root of its project, with no
 *  document. */
const issue: IssueDetails = {
  ...details,
  assignee: "alice",
  primary_artifact_id: "",
  title: "Pin the issue from its header",
};

/** The viewer's state of `issue` before the pin: unpinned. */
const unpinnedState = stateForIssue(undefined, issue.key);

/** The viewer's state as Dispatch holds it before the pin. */
const unpinned: UserState = { [issue.key]: unpinnedState };

/** The pin's PUT's answer: Dispatch's state of the issue once it took the pin. */
const pinnedState: UserIssueState = { ...unpinnedState, pinned: true, seq: 1 };

/** The header given the viewer's state of `issue`. */
function Header({ state }: { state: UserIssueState }): ReactNode {
  return (
    <IssueHeader
      documentArtifact={undefined}
      isClosed={false}
      issue={issue}
      priorityWrite={idlePriorityWrite}
      state={state}
    />
  );
}

/** The header as the issue page draws it: given the viewer's state of the issue from the page's
 *  one read of that state (`useIssueDetail`). */
function PageHeader(): ReactNode {
  const userState = useQuery(userStateQuery());
  return <Header state={stateForIssue(userState.data, issue.key)} />;
}

/** `ui` on `client`, signed in as alice, with every read of the viewer's state (`getMyState`) and
 *  the pin's PUT (`pinSave`) held until the test answers it. The header's other reads find a
 *  project with no repository mapping, architecture source or subscriber. */
function renderHeader(client: QueryClient, ui: ReactNode) {
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const restoreReads = mockHeaderReads();
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const reads: ReturnType<typeof Promise.withResolvers<UserState>>[] = [];
  const getMyState = spyOn(api, "getMyState").mockImplementation(() => {
    const read = Promise.withResolvers<UserState>();
    reads.push(read);
    return read.promise;
  });
  const pinSave = Promise.withResolvers<UserIssueState>();
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(pinSave.promise);
  const view = render(ui, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <MarginProvider>{children}</MarginProvider>
        </QueryClientProvider>
      </MemoryRouter>
    ),
  });
  return {
    getMyState,
    pinSave,
    putIssueState,
    q: within(view.container),
    reads,
    unmount: () => {
      view.unmount();
      restoreReads();
      getMyState.mockRestore();
      putIssueState.mockRestore();
      subscribers.mockRestore();
    },
    user: userEvent.setup(),
  };
}

// The pin's answer went into the cache under a read of the viewer's state asked while the PUT was
// out, and that read's answer, of the state from before Dispatch took the pin, landed over it: the
// issue read as unpinned again wherever the state was next drawn. Nothing reads the state here,
// as when the reader has left the page, so the pin's settling does not read it again over that.
test("a read of the state asked while the header's pin is out does not put back the unpinned state its answer replaced", async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = renderHeader(client, <Header state={unpinnedState} />);
  try {
    const first = client.fetchQuery(userStateQuery());
    await act(async () => {
      view.reads[0]?.resolve(unpinned);
      await first;
    });
    await view.user.click(view.q.getByRole("button", { name: "Pin issue" }));
    await waitFor(() => expect(view.putIssueState).toHaveBeenCalledTimes(1));
    // While the PUT is out the state is read again; Dispatch has not taken the pin yet.
    act(() => {
      void client.refetchQueries({ queryKey: userStateQuery().queryKey });
    });
    await waitFor(() => expect(view.getMyState).toHaveBeenCalledTimes(2));

    await act(async () => {
      view.pinSave.resolve(pinnedState);
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await act(async () => {
      view.reads[1]?.resolve(unpinned);
      await settle();
    });
    expect(client.getQueryData<UserState>(userStateQuery().queryKey)?.[issue.key]).toEqual(
      pinnedState
    );
  } finally {
    view.unmount();
  }
});

// A refresh of the viewer's state asked while the header's pin was out could reach Dispatch
// before the pin did, and its answer turned the pin back off under the reader's press. The read
// waits for the pin's answer and is asked once it is in, so the header shows what Dispatch holds
// after the pin, its pin no longer held.
test("a read of the state asked while the header's pin is out waits for its answer, and the header shows what it reads then", async () => {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  // Each read of the state, listed as it calls its function: a read the pin holds is listed
  // before it waits, and one that is not held has asked by then.
  const asked: string[] = [];
  const stopListening = client.onAttempt((queryHash) => {
    if (queryHash === hashKey(userStateQuery().queryKey)) asked.push(queryHash);
  });
  const view = renderHeader(client, <PageHeader />);
  // Dispatch's state once it took the pin, with a read the reader made elsewhere since.
  const after: UserIssueState = { ...pinnedState, last_read_seq: 7 };
  try {
    await waitFor(() => expect(view.getMyState).toHaveBeenCalledTimes(1));
    await act(async () => {
      view.reads[0]?.resolve(unpinned);
    });
    const pin = await view.q.findByRole("button", { name: "Pin issue" });
    expect(pin).toHaveAttribute("aria-pressed", "false");

    await view.user.click(pin);
    await waitFor(() => expect(view.putIssueState).toHaveBeenCalledTimes(1));
    expect(view.q.getByRole("button", { name: "Unpin issue" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    await act(async () => {
      void client.invalidateQueries({ queryKey: userStateQuery().queryKey });
    });
    // The refresh is asked, and waits: the state is read once, before the pin.
    await waitFor(() => expect(asked).toHaveLength(2));
    expect(view.getMyState).toHaveBeenCalledTimes(1);
    expect(view.q.getByRole("button", { name: "Unpin issue" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    await act(async () => {
      view.pinSave.resolve(pinnedState);
    });
    await waitFor(() => expect(view.getMyState).toHaveBeenCalledTimes(2));
    view.getMyState.mockResolvedValue({ [issue.key]: after });
    await act(async () => {
      view.reads[1]?.resolve({ [issue.key]: after });
    });
    await waitFor(() =>
      expect(client.getQueryData<UserState>(userStateQuery().queryKey)?.[issue.key]).toEqual(after)
    );
    await waitFor(() => expect(client.isMutating()).toBe(0));
    const shown = view.q.getByRole("button", { name: "Unpin issue" });
    expect(shown).toHaveAttribute("aria-pressed", "true");
    expect(shown).not.toHaveAttribute("aria-disabled");
  } finally {
    view.unmount();
    stopListening();
  }
});

// The header's `a`, read with the keymap the shell mounts and the `issue` scope the page pushes.

/** The issue's spec, with its approval request (`ask-approval`) awaiting version 2. */
const awaitingSpec: Artifact = {
  approval: { ask_id: "ask-approval", latest_version: 2, state: "awaiting" },
  created_at: "2026-09-24T05:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: base.key,
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: base.project,
  ref_key: "CORE-1/spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

/** The issue with its spec's approval request open, its turn alice's: the reader's. */
const approvalIssue: IssueDetails = {
  ...details,
  artifacts: [awaitingSpec],
  open_asks: [
    {
      anchor: null,
      answer: null,
      artifact_id: awaitingSpec.id,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      author: { id: "session-1", kind: "session" },
      block_id: null,
      created_at: "2026-09-24T05:30:00Z",
      edited_at: null,
      id: "ask-approval",
      issue_key: base.key,
      kind: "approval",
      last_reply: null,
      multiple: false,
      opened_event_id: 1,
      options: [],
      question: "Approve the spec?",
      state: "open",
      urgency: "med",
      waiting_on: "human",
      waiting_on_person: "alice",
    },
  ],
};

/** Dispatch's answer to the approval of version 2 of the spec. */
const specApproval: ArtifactReview = {
  actor: { id: "alice", kind: "user" },
  artifact_id: awaitingSpec.id,
  ask_id: "ask-approval",
  content_sha256: null,
  content_sha256_source: null,
  created_at: "2026-09-24T06:30:00Z",
  id: "review-1",
  kind: "person",
  reason: null,
  state: "approved",
  version: 2,
};

/** Pushes the `issue` scope, which the issue page pushes and the header's keys fire under. */
function IssueScope(): null {
  useKeymapScope("issue");
  return null;
}

/** The header of the issue the cache holds, given its spec from that same read, as `IssuePage`
 *  draws it. */
function SpecHeader(): ReactNode {
  const read = useQuery({ ...issueQuery(base.key), staleTime: Infinity }).data;
  if (read === undefined) return null;
  return (
    <IssueHeader
      documentArtifact={read.artifacts?.[0]}
      isClosed={false}
      issue={read}
      priorityWrite={idlePriorityWrite}
      state={stateForIssue(undefined, read.key)}
    />
  );
}

// `a` approves once per press. A held `a` repeats its keydown, and a repeat presses nothing; and
// Approve stays held from the press until the issue is read again, so neither a repeat nor a
// second `a` before that read records a second approval, which Dispatch would record at the
// latest version once the request it answered is closed.
test("a held a, or a second a before the issue is read again, records one approval", async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(issueQuery(base.key).queryKey, approvalIssue);
  const restoreReads = mockHeaderReads();
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const readAgain = Promise.withResolvers<IssueDetails>();
  const getIssue = spyOn(api, "getIssue").mockReturnValue(readAgain.promise);
  const review = spyOn(api, "createArtifactVersionReview").mockResolvedValue(specApproval);
  const answered: IssueDetails = {
    ...approvalIssue,
    artifacts: [
      { ...awaitingSpec, approval: { latest_version: 2, state: "approved", version: 2 } },
    ],
    open_asks: [],
  };
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>
          <KeymapProvider>
            <IssueScope />
            <SpecHeader />
          </KeymapProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const approve = await screen.findByRole("button", { name: "Approve" });
    // A repeat presses nothing, though Approve takes a press. user-event has no form for this: it
    // sends a repeat only after the key's first keydown.
    fireEvent.keyDown(document.body, { key: "a", repeat: true });
    await act(settle);
    expect(review).not.toHaveBeenCalled();

    await user.keyboard("a");
    await waitFor(() => expect(review).toHaveBeenCalledTimes(1));
    expect(review).toHaveBeenCalledWith("artifact-1", 2, { state: "approved" }, keyedAttempt);
    // Dispatch recorded it, and the issue is being read again: `a` held down and `a` pressed
    // again record nothing more.
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
    await user.keyboard("{a>3/}");
    await user.keyboard("a");
    await act(settle);
    expect(review).toHaveBeenCalledTimes(1);
    expect(approve).toHaveAttribute("aria-disabled", "true");

    // The read lands with the request answered, and the header draws no Approve.
    await act(async () => readAgain.resolve(answered));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
    );
    await user.keyboard("a");
    expect(review).toHaveBeenCalledTimes(1);
  } finally {
    readAgain.resolve(answered);
    view.unmount();
    restoreReads();
    subscribers.mockRestore();
    getIssue.mockRestore();
    review.mockRestore();
  }
});

// The four Set priority rows have no key until a person gives them one. A held key repeats its
// keydown, and only its first press writes: each repeat would be one more write of the same
// priority, sent behind the first.
test("a held key a person gave Set priority P2 writes the priority once", async () => {
  setActionKeys("alice", "issue.set-p2", ["Shift+Z"]);
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const restoreReads = mockHeaderReads();
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  // Each priority the header's write is given.
  const submitted: (number | null)[] = [];
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>
          <KeymapProvider login="alice">
            <IssueScope />
            <IssueHeader
              documentArtifact={undefined}
              isClosed={false}
              issue={details}
              priorityWrite={{ ...idlePriorityWrite, submit: (level) => submitted.push(level) }}
              state={stateForIssue(undefined, details.key)}
            />
          </KeymapProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByRole("heading", { level: 1, name: base.title });
    await user.keyboard("{Shift>}{Z>4/}{/Shift}");
    expect(submitted).toEqual([2]);
  } finally {
    view.unmount();
    restoreReads();
    subscribers.mockRestore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "shell.keymap"));
  }
});
