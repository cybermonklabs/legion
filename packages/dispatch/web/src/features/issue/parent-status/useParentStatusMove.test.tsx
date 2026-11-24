import { expect, jest, type Mock, mock, spyOn, test } from "bun:test";
import { NO_ROUTE_ROLLING_RELEASE_MESSAGE } from "@legion/contracts";
import { QueryClient, QueryClientProvider, skipToken, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, type NavigateFunction, useNavigate } from "react-router-dom";

import { realClock, settle, tick } from "../../../__tests__/fake-clock";
import { keyedAttempt } from "../../../__tests__/keyed-attempt";
import { answerRequests } from "../../../__tests__/network";
import { ApiError } from "../../../api/client";
import type { IssueDetails, StatusChangeAnswer, StatusChangeInput } from "../../../api/types";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { refusedNote } from "../../writes/useKeyedWrite";
import { ParentStatusDialog } from "./ParentStatusDialog";
import {
  answerStatusChanges,
  fixtureAnswer,
  fixtureRefusal,
  type StatusChangeAnswerer,
} from "./status-change-fixtures";
import {
  issueHasParentOrOpenChild,
  type ParentStatusMove,
  type StatusMove,
  type StatusMoveEnd,
  useParentStatusMove,
} from "./useParentStatusMove";

// The page's state table of the parent-status question (INFRA-4731's plan, "Lists named before
// code"): each row is one state and one event. The route's answers are the ones the real server
// wrote (`status-change-fixtures.ts`): ACME-4 closed with a child in work (`conflict`), its
// `include_children` dry run (`dry-run`), and so on.

/** Which request of the route `input` is: the dry run of the move itself, the dry run of one
 *  option, or a write with one option. */
function requestOf(input: StatusChangeInput): string {
  const option = input.include_children
    ? "include_children"
    : input.unlink_children
      ? "unlink_children"
      : input.include_parent
        ? "include_parent"
        : "no option";
  return `${input.dry_run === true ? "dry run" : "write"} of ${input.status} with ${option}`;
}

/** Answers each request of the route by `requestOf`; a request the table names no answer for
 *  fails the test. */
function answerBy(table: Readonly<Record<string, () => Promise<StatusChangeAnswer>>>) {
  const statusChanges = answerStatusChanges();
  statusChanges.mockImplementation(async (_key, input) => {
    const answer = table[requestOf(input)];
    if (answer === undefined) throw new Error(`no answer for the ${requestOf(input)}`);
    return answer();
  });
  return statusChanges;
}

/** Each request the route was sent, as `requestOf` names it, in order. */
function requests(statusChanges: Mock<StatusChangeAnswerer>): string[] {
  return statusChanges.mock.calls.map(([, input]) => requestOf(input));
}

const never = () => new Promise<StatusChangeAnswer>(() => {});
const refuse = (refusal: ApiError) => () => Promise.reject(refusal);

/** A surface that makes its status moves through the hook, as the issue header does, with the
 *  pending status, the refusal and the question drawn as the header draws them. It is drawn under
 *  a router, as every page is: `navigate` moves the page under it, as a link would. */
function renderSurface() {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  let current: ParentStatusMove | undefined;
  let navigateTo: NavigateFunction | undefined;
  function Surface(): ReactNode {
    const parentStatus = useParentStatusMove();
    current = parentStatus;
    navigateTo = useNavigate();
    return (
      <>
        <output aria-label="Pending status">{parentStatus.pending?.status ?? ""}</output>
        {parentStatus.refusal === undefined ? null : (
          <p role="alert">{refusedNote("change the status of ACME-4", parentStatus.refusal)}</p>
        )}
        {parentStatus.question === undefined ? null : (
          <ParentStatusDialog question={parentStatus.question} />
        )}
      </>
    );
  }
  // The dialog's Escape is a binding of the page's keymap, which `KeymapProvider` listens for.
  const view = render(
    <MemoryRouter initialEntries={["/issues/ACME-4"]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Surface />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const hook = (): ParentStatusMove => {
    if (current === undefined) throw new Error("the surface has not rendered");
    return current;
  };
  /** Makes `move` and lets every answer already given reach the page. The move's end is handed
   *  back inside an object: an async function would wait for a promise it returned bare. */
  const start = async (move: StatusMove): Promise<{ ended: Promise<StatusMoveEnd> }> => {
    let ended: Promise<StatusMoveEnd> = Promise.resolve({ kind: "busy" });
    await act(async () => {
      ended = hook().move(move);
      await settle();
    });
    return { ended };
  };
  const navigate = (to: string) =>
    act(() => {
      if (navigateTo === undefined) throw new Error("the surface has not rendered");
      void navigateTo(to);
    });
  return { client, hook, navigate, start, unmount: view.unmount };
}

/** ACME-4, which has a child in work, picked for `done`. */
function closeAcme4(patch = mock(() => {})): StatusMove & { patch: typeof patch } {
  return { key: "ACME-4", patch, related: true, status: "done" };
}

const pendingStatus = () => screen.getByRole("status", { name: "Pending status" });
const question = () => screen.getByRole("dialog", { name: "Move ACME-4 to Done?" });
const choice = (name: string) => within(question()).getByRole("button", { name });

test("idle: a status picked for an issue with no parent and no open child sends the PATCH as today, and no dry run", async () => {
  const statusChanges = answerStatusChanges();
  const surface = renderSurface();
  try {
    const move = { ...closeAcme4(), related: false };
    const { ended } = await surface.start(move);
    expect(await ended).toEqual({ kind: "patched" });
    expect(move.patch).toHaveBeenCalledTimes(1);
    expect(statusChanges).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

test("idle: a status picked for an issue with a parent or an open child shows it as pending and sends the dry run", async () => {
  const statusChanges = answerBy({ "dry run of done with no option": never });
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    await surface.start(move);
    expect(statusChanges).toHaveBeenCalledWith(
      "ACME-4",
      { dry_run: true, status: "done" },
      expect.objectContaining({ ...keyedAttempt, login: "alice" })
    );
    expect(pendingStatus()).toHaveTextContent("done");
    expect(move.patch).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

test("checking: the dry run answers 200, and the PATCH goes as today", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": async () => fixtureAnswer("dry-run"),
  });
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    const { ended } = await surface.start(move);
    expect(await ended).toEqual({ kind: "patched" });
    expect(move.patch).toHaveBeenCalledTimes(1);
    expect(statusChanges.mock.invocationCallOrder[0]).toBeLessThan(
      move.patch.mock.invocationCallOrder[0] ?? 0
    );
    expect(requests(statusChanges)).toEqual(["dry run of done with no option"]);
    expect(pendingStatus()).toHaveTextContent("");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  } finally {
    surface.unmount();
  }
});

test("checking: the dry run answers 409 PARENT_STATUS_CONFLICT, and the dialog lists the issues and sends each choice's dry run", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": never,
    "dry run of done with unlink_children": never,
  });
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    await surface.start(move);
    const listed = within(question()).getByRole("list", { name: "Issues that stop this change" });
    expect(
      within(listed)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual(["ACME-5 Child in work (child, Todo)"]);
    // A listed child offers the children's two choices, and no parent's.
    expect(choice("Move them with it")).toBeDisabled();
    expect(choice("Unlink them")).toBeDisabled();
    expect(
      within(question()).queryByRole("button", { name: "Move the parents into work too" })
    ).not.toBeInTheDocument();
    expect(requests(statusChanges)).toEqual([
      "dry run of done with no option",
      "dry run of done with include_children",
      "dry run of done with unlink_children",
    ]);
    expect(pendingStatus()).toHaveTextContent("done");
    expect(move.patch).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

// The recorded conflicts all name a child, so the parent's case is the recorded conflict with its
// one issue named as the parent instead (the epic of `changed.json`, on the backlog).
test("checking: a 409 that names the parent offers to move the parents into work too", async () => {
  const conflict = fixtureRefusal("conflict");
  const parentConflict = new ApiError(409, {
    changed: [],
    code: conflict.code,
    error: conflict.message,
    issues: [
      { key: "ACME-1", relation: "parent", status: "backlog", title: "Epic on the backlog" },
    ],
  });
  const statusChanges = answerBy({
    "dry run of in_progress with no option": refuse(parentConflict),
    "dry run of in_progress with include_parent": async () => fixtureAnswer("changed"),
  });
  const surface = renderSurface();
  try {
    await surface.start({ key: "ACME-2", patch: () => {}, related: true, status: "in_progress" });
    const dialog = screen.getByRole("dialog", { name: "Move ACME-2 to In progress?" });
    expect(
      within(dialog).getByRole("button", { name: "Move the parents into work too" })
    ).toBeEnabled();
    expect(
      within(dialog).queryByRole("button", { name: "Move them with it" })
    ).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Unlink them" })).not.toBeInTheDocument();
    expect(requests(statusChanges)).toEqual([
      "dry run of in_progress with no option",
      "dry run of in_progress with include_parent",
    ]);
  } finally {
    surface.unmount();
  }
});

/** The refusals of a dry run that are not the rule's: each one as the real server wrote it where
 *  a fixture holds it, and the route's own words for a status it does not know. */
const otherRefusals: readonly [string, () => ApiError][] = [
  ["403 BROWSER_SIGN_IN_REQUIRED", () => fixtureRefusal("browser-only")],
  ["409 ISSUE_CHANGED", () => fixtureRefusal("stale")],
  [
    "400 INVALID_STATUS",
    () =>
      new ApiError(400, {
        changed: [],
        code: "INVALID_STATUS",
        error: "status is not in the Legion lifecycle",
      }),
  ],
];

for (const [name, refusal] of otherRefusals) {
  test(`checking: the dry run answers ${name}, so nothing is written, the refusal shows and the status goes back`, async () => {
    const answer = refusal();
    answerBy({ "dry run of done with no option": refuse(answer) });
    const surface = renderSurface();
    try {
      const move = closeAcme4();
      const { ended } = await surface.start(move);
      expect(await ended).toEqual({ kind: "refused", refusal: answer });
      expect(screen.getByRole("alert")).toHaveTextContent(
        `Couldn't change the status of ACME-4 — ${answer.message}`
      );
      expect(pendingStatus()).toHaveTextContent("");
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(move.patch).not.toHaveBeenCalled();
    } finally {
      surface.unmount();
    }
  });
}

// A Dispatch process of an older build answers no route for the route; the client sends it again
// after each of its waits, and then refuses with its own words.
test("checking: the dry run answers no route after the client's retries, so nothing is written and the status goes back", async () => {
  jest.useFakeTimers();
  const attempts: string[] = [];
  answerRequests(
    http.post("/api/v1/issues/:key/status-change", ({ params }) => {
      attempts.push(String(params.key));
      return Response.json(
        {
          code: "NOT_FOUND",
          error: "no route for POST /api/v1/issues/ACME-4/status-change",
        },
        { status: 404 }
      );
    })
  );
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    const { ended } = await surface.start(move);
    // The waits are 1, 2 and 4 seconds, each up to a quarter longer.
    await tick(10_000);
    expect(attempts).toEqual(["ACME-4", "ACME-4", "ACME-4", "ACME-4"]);
    expect(await ended).toMatchObject({ kind: "refused" });
    expect(screen.getByRole("alert")).toHaveTextContent(NO_ROUTE_ROLLING_RELEASE_MESSAGE);
    expect(pendingStatus()).toHaveTextContent("");
    expect(move.patch).not.toHaveBeenCalled();
  } finally {
    realClock();
    surface.unmount();
  }
});

/** ACME-4 as the issue page reads it: under no parent, with ACME-5, a child in work. */
const acme4WithChildInWork: Pick<IssueDetails, "children" | "parent"> = {
  children: [
    {
      active_at: "2026-09-09T00:00:00Z",
      external_links: [],
      key: "ACME-5",
      status: "todo",
      subtree_done: 0,
      subtree_total: 1,
      title: "Child in work",
    },
  ],
  parent: null,
};

/** The issue page's Close of ACME-4, as the page makes it: it reads the issue from the cache, which
 *  an event of the issue refreshes, and takes whether it has a parent or an open child from that
 *  read at the press (`issueHasParentOrOpenChild`). */
function CloseFromTheCache({ patch }: { patch: () => void }): ReactNode {
  const parentStatus = useParentStatusMove();
  // The test writes the issue into the cache as its read and its event would; nothing fetches it.
  const issue = useQuery<Pick<IssueDetails, "children" | "parent">>({
    queryFn: skipToken,
    queryKey: ["issue", "ACME-4"],
  }).data;
  if (issue === undefined) return null;
  const related = issueHasParentOrOpenChild(issue);
  return (
    <>
      <output aria-label="Parent or open child">{related ? "yes" : "no"}</output>
      <button
        onClick={() => void parentStatus.move({ key: "ACME-4", patch, related, status: "done" })}
        type="button"
      >
        Close ACME-4
      </button>
      {parentStatus.question === undefined ? null : (
        <ParentStatusDialog question={parentStatus.question} />
      )}
    </>
  );
}

// The page knew ACME-4 had a child in work, so the close sent the dry run. While it is out, the
// issue's event shows the child done, which would let a press made now go straight to the PATCH.
// The move in flight still goes as its dry run answers: the answer, not the page's read, decides.
test("checking: a live event changes the issue, and the dry run's answer is still used", async () => {
  const held = Promise.withResolvers<StatusChangeAnswer>();
  const statusChanges = answerBy({
    "dry run of done with no option": () => held.promise,
    "dry run of done with include_children": never,
    "dry run of done with unlink_children": never,
  });
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(["issue", "ACME-4"], acme4WithChildInWork);
  const patch = mock(() => {});
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <CloseFromTheCache patch={patch} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const related = () => screen.getByRole("status", { name: "Parent or open child" });
  try {
    await user.click(screen.getByRole("button", { name: "Close ACME-4" }));
    expect(requests(statusChanges)).toEqual(["dry run of done with no option"]);
    // The issue's event refreshes what the page reads of it: its child is done now.
    act(() => {
      client.setQueryData(["issue", "ACME-4"], {
        ...acme4WithChildInWork,
        children: (acme4WithChildInWork.children ?? []).map((child) => ({
          ...child,
          status: "done",
        })),
      });
    });
    expect(related()).toHaveTextContent("no");
    await act(async () => {
      held.reject(fixtureRefusal("conflict"));
      await settle();
    });
    // The move asks as its dry run answered, and checks nothing again; Dispatch judges the write.
    expect(question()).toBeInTheDocument();
    expect(requests(statusChanges)).toEqual([
      "dry run of done with no option",
      "dry run of done with include_children",
      "dry run of done with unlink_children",
    ]);
    expect(patch).not.toHaveBeenCalled();
    // A press made after the question closes reads the issue as the event left it: no dry run.
    await user.click(choice("Cancel"));
    await user.click(screen.getByRole("button", { name: "Close ACME-4" }));
    expect(patch).toHaveBeenCalledTimes(1);
    expect(requests(statusChanges)).toHaveLength(3);
  } finally {
    view.unmount();
  }
});

test("asking: a choice's dry run answers, and the choice lists its changes and can be confirmed", async () => {
  answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
  });
  const surface = renderSurface();
  try {
    await surface.start(closeAcme4());
    const changes = within(question()).getByRole("list", {
      name: "What Move them with it changes",
    });
    expect(
      within(changes)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual([
      "ACME-6: Done, under ACME-5",
      "ACME-5: Done, under ACME-4",
      "ACME-4: Done, no parent",
    ]);
    expect(choice("Move them with it")).toBeEnabled();
  } finally {
    surface.unmount();
  }
});

test("asking: a choice whose dry run is still out cannot be confirmed yet", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
  });
  const user = userEvent.setup();
  const surface = renderSurface();
  try {
    await surface.start(closeAcme4());
    const unlink = choice("Unlink them");
    expect(unlink).toBeDisabled();
    expect(within(question()).getByText("Checking what this changes…")).toBeInTheDocument();
    await user.click(unlink);
    expect(requests(statusChanges)).not.toContain("write of done with unlink_children");
    expect(question()).toBeInTheDocument();
  } finally {
    surface.unmount();
  }
});

/** The three ways out of the question that write nothing. */
const closes: readonly [string, (user: UserEvent) => Promise<void>][] = [
  ["Keep as it is", (user) => user.click(choice("Keep as it is"))],
  ["Cancel", (user) => user.click(choice("Cancel"))],
  ["Escape", (user) => user.keyboard("{Escape}")],
];

for (const [name, close] of closes) {
  test(`asking: ${name} closes the question, writes nothing and puts the status back`, async () => {
    const statusChanges = answerBy({
      "dry run of done with no option": refuse(fixtureRefusal("conflict")),
      "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
      "dry run of done with unlink_children": never,
    });
    const user = userEvent.setup();
    const surface = renderSurface();
    try {
      const move = closeAcme4();
      const { ended } = await surface.start(move);
      await close(user);
      expect(await ended).toEqual({ kind: "kept" });
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(pendingStatus()).toHaveTextContent("");
      expect(requests(statusChanges).filter((request) => request.startsWith("write"))).toEqual([]);
      expect(move.patch).not.toHaveBeenCalled();
    } finally {
      surface.unmount();
    }
  });
}

// A link or the browser's Back moves the page under the question: it closes as Keep as it is
// does, as the page's other dialogs close on a navigation (`useCloseOnNavigation`).
test("asking: a navigation closes the question, writes nothing and puts the status back", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
  });
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    const { ended } = await surface.start(move);
    expect(question()).toBeInTheDocument();
    surface.navigate("/issues/ACME-5");
    expect(await ended).toEqual({ kind: "kept" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(pendingStatus()).toHaveTextContent("");
    expect(requests(statusChanges).filter((request) => request.startsWith("write"))).toEqual([]);
    expect(move.patch).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

// A navigation that takes the surface away ends its question as Keep as it is would, so the move
// ends and nothing waits on it.
test("asking: the surface going away ends the move as kept, and writes nothing", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": never,
    "dry run of done with unlink_children": never,
  });
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    const { ended } = await surface.start(move);
    expect(question()).toBeInTheDocument();
    surface.unmount();
    expect(await ended).toEqual({ kind: "kept" });
    expect(requests(statusChanges).filter((request) => request.startsWith("write"))).toEqual([]);
    expect(move.patch).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

/** Answers the move's own dry run with `answer`, once the test lets it go (`answerNow`). */
function holdTheDryRun(answer: () => Promise<StatusChangeAnswer>) {
  const held = Promise.withResolvers<void>();
  const statusChanges = answerBy({
    "dry run of done with no option": async () => {
      await held.promise;
      return answer();
    },
  });
  return { answerNow: () => held.resolve(), statusChanges };
}

// The person's pick needs no answer of theirs where the rule asks nothing, so it is saved even
// though the surface went away while its dry run was out.
test("checking: a dry run answered 200 once the surface has gone still sends the PATCH, once", async () => {
  const { answerNow, statusChanges } = holdTheDryRun(async () => fixtureAnswer("dry-run"));
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    const { ended } = await surface.start(move);
    surface.unmount();
    answerNow();
    expect(await ended).toEqual({ kind: "patched" });
    expect(requests(statusChanges)).toEqual(["dry run of done with no option"]);
    expect(move.patch).toHaveBeenCalledTimes(1);
  } finally {
    surface.unmount();
  }
});

// Where the rule would refuse the pick, only the person could say what to do, and they have gone
// with the surface: the question never opens, and nothing is written.
test("checking: a dry run answered 409 PARENT_STATUS_CONFLICT once the surface has gone ends the move as kept, and writes nothing", async () => {
  const { answerNow, statusChanges } = holdTheDryRun(refuse(fixtureRefusal("conflict")));
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    const { ended } = await surface.start(move);
    surface.unmount();
    answerNow();
    expect(await ended).toEqual({ kind: "kept" });
    expect(requests(statusChanges)).toEqual(["dry run of done with no option"]);
    expect(move.patch).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

// The write store keeps a write going once its page has gone, so the rule may refuse it then:
// nobody is left to ask, and the refusal stays the shell's indicator's to show.
test("idle: a PATCH refused under the rule once the surface has gone asks nothing", async () => {
  const statusChanges = answerStatusChanges();
  const surface = renderSurface();
  try {
    const { ask } = surface.hook();
    surface.unmount();
    expect(await ask({ key: "ACME-4", status: "done" }, fixtureRefusal("conflict"))).toEqual({
      kind: "kept",
    });
    expect(statusChanges).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

test("asking: a choice confirmed sends the status change with that option", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
    "write of done with include_children": never,
  });
  const user = userEvent.setup();
  const surface = renderSurface();
  try {
    const move = closeAcme4();
    await surface.start(move);
    await user.click(choice("Move them with it"));
    await waitFor(() =>
      expect(statusChanges).toHaveBeenLastCalledWith(
        "ACME-4",
        { include_children: true, status: "done" },
        keyedAttempt
      )
    );
    // While it is out, nothing else of the question can be pressed.
    expect(choice("Saving…")).toBeDisabled();
    expect(choice("Cancel")).toBeDisabled();
    expect(move.patch).not.toHaveBeenCalled();
  } finally {
    surface.unmount();
  }
});

test("writing: answered 200, the question closes and each changed issue and the list are read again", async () => {
  const answer = fixtureAnswer("changed");
  answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
    "write of done with include_children": async () => answer,
  });
  const user = userEvent.setup();
  const surface = renderSurface();
  const invalidated = spyOn(surface.client, "invalidateQueries");
  try {
    const { ended } = await surface.start(closeAcme4());
    await user.click(choice("Move them with it"));
    expect(await ended).toEqual({ answer, kind: "changed" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(pendingStatus()).toHaveTextContent("");
    expect(invalidated.mock.calls.map(([filters]) => filters?.queryKey)).toEqual([
      ["issue", "ACME-1"],
      ["issue", "ACME-3"],
      ["issue", "ACME-2"],
      ["issues"],
    ]);
  } finally {
    invalidated.mockRestore();
    surface.unmount();
  }
});

test("writing: answered 409 PARENT_STATUS_CONFLICT with what changed, the question names it and the new conflict, and checks each choice again", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
    "write of done with include_children": refuse(fixtureRefusal("conflict-midway")),
  });
  const user = userEvent.setup();
  const surface = renderSurface();
  try {
    await surface.start(closeAcme4());
    await user.click(choice("Move them with it"));
    const made = await within(question()).findByRole("list", { name: "Changes already made" });
    expect(
      within(made)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual(["ACME-8: Done, under ACME-7"]);
    const listed = within(question()).getByRole("list", { name: "Issues that stop this change" });
    expect(
      within(listed)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual(["ACME-10 Filed between two steps (child, Triage)"]);
    await waitFor(() => expect(choice("Move them with it")).toBeEnabled());
    expect(requests(statusChanges)).toEqual([
      "dry run of done with no option",
      "dry run of done with include_children",
      "dry run of done with unlink_children",
      "write of done with include_children",
      "dry run of done with include_children",
      "dry run of done with unlink_children",
    ]);
  } finally {
    surface.unmount();
  }
});

test("writing: answered another refusal, the question shows it, writes nothing more, and Cancel closes it", async () => {
  const refusal = fixtureRefusal("browser-only");
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
    "write of done with include_children": refuse(refusal),
  });
  const user = userEvent.setup();
  const surface = renderSurface();
  try {
    const { ended } = await surface.start(closeAcme4());
    await user.click(choice("Move them with it"));
    expect(await within(question()).findByRole("alert")).toHaveTextContent(refusal.message);
    expect(choice("Move them with it")).toBeDisabled();
    await user.click(choice("Move them with it"));
    expect(requests(statusChanges).filter((request) => request.startsWith("write"))).toEqual([
      "write of done with include_children",
    ]);
    await user.click(choice("Cancel"));
    expect(await ended).toEqual({ kind: "kept" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  } finally {
    surface.unmount();
  }
});

test("idle: a PATCH answered 409 PARENT_STATUS_CONFLICT opens the question as a 409 dry run does", async () => {
  const statusChanges = answerBy({
    "dry run of done with include_children": async () => fixtureAnswer("dry-run"),
    "dry run of done with unlink_children": never,
  });
  const surface = renderSurface();
  try {
    await act(async () => {
      void surface.hook().ask({ key: "ACME-4", status: "done" }, fixtureRefusal("conflict"));
      await settle();
    });
    expect(within(question()).getByText("ACME-5")).toBeInTheDocument();
    expect(choice("Move them with it")).toBeEnabled();
    expect(pendingStatus()).toHaveTextContent("done");
    expect(requests(statusChanges)).toEqual([
      "dry run of done with include_children",
      "dry run of done with unlink_children",
    ]);
  } finally {
    surface.unmount();
  }
});

// The issue page takes a second pick while the first is out, as it takes one while a write is out
// (its select is never disabled), so the hook drops none: the second shows at once and goes once
// the first has made its write, in the order the two came.
test("checking: a second move while one is checking shows at once, and goes once the first has made its write", async () => {
  const first = Promise.withResolvers<StatusChangeAnswer>();
  const statusChanges = answerBy({
    "dry run of done with no option": () => first.promise,
    "dry run of backlog with no option": async () => fixtureAnswer("dry-run"),
  });
  const surface = renderSurface();
  try {
    const closing = closeAcme4();
    const { ended: closed } = await surface.start(closing);
    const shelving = { ...closeAcme4(), status: "backlog" };
    const { ended: shelved } = await surface.start(shelving);
    expect(pendingStatus()).toHaveTextContent("backlog");
    expect(requests(statusChanges)).toEqual(["dry run of done with no option"]);
    await act(async () => {
      first.resolve(fixtureAnswer("dry-run"));
      expect(await closed).toEqual({ kind: "patched" });
      expect(await shelved).toEqual({ kind: "patched" });
    });
    expect(requests(statusChanges)).toEqual([
      "dry run of done with no option",
      "dry run of backlog with no option",
    ]);
    expect(closing.patch.mock.invocationCallOrder[0]).toBeLessThan(
      shelving.patch.mock.invocationCallOrder[0] ?? 0
    );
    expect(pendingStatus()).toHaveTextContent("");
  } finally {
    surface.unmount();
  }
});

test("asking: a second move while the question is open does nothing", async () => {
  const statusChanges = answerBy({
    "dry run of done with no option": refuse(fixtureRefusal("conflict")),
    "dry run of done with include_children": never,
    "dry run of done with unlink_children": never,
  });
  const surface = renderSurface();
  try {
    await surface.start(closeAcme4());
    expect(question()).toBeInTheDocument();
    const second = { ...closeAcme4(), status: "backlog" };
    expect(await (await surface.start(second)).ended).toEqual({ kind: "busy" });
    expect(requests(statusChanges)).toEqual([
      "dry run of done with no option",
      "dry run of done with include_children",
      "dry run of done with unlink_children",
    ]);
    expect(second.patch).not.toHaveBeenCalled();
    expect(pendingStatus()).toHaveTextContent("done");
  } finally {
    surface.unmount();
  }
});
