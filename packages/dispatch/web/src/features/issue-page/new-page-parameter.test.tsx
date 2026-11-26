import { expect, test } from "bun:test";
import { act, type RenderResult, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useContext } from "react";
import {
  Link,
  MemoryRouter,
  type NavigateFunction,
  type NavigateOptions,
  type Navigator,
  UNSAFE_NavigationContext,
  useLocation,
  useNavigate,
} from "react-router-dom";

import { KeepNewIssuePage } from "./new-page-parameter";

/** The address of the issue's activity, the target of the one link the probe draws. */
const ACTIVITY = "/issues/CORE-1/activity";

/** The `navigate` of the tree the row drew, for a move the page makes itself, and the navigator
 *  the rule hands on, for a push no `navigate` has resolved. */
const held: { navigate?: NavigateFunction; navigator?: Navigator } = {};

/** The address the router is at, and one link to the issue's activity, both under the rule. */
function Probe(): ReactNode {
  const { hash, pathname, search } = useLocation();
  held.navigate = useNavigate();
  held.navigator = useContext(UNSAFE_NavigationContext).navigator;
  return (
    <>
      <output aria-label="Address">{`${pathname}${search}${hash}`}</output>
      <Link to={ACTIVITY}>Activity</Link>
    </>
  );
}

/** Draws the probe under the rule, in a router whose one entry is `path`. */
function drawAt(path: string): RenderResult {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <KeepNewIssuePage>
        <Probe />
      </KeepNewIssuePage>
    </MemoryRouter>
  );
}

function address(): string | null {
  return screen.getByRole("status", { name: "Address" }).textContent;
}

/** The `href` the link to the activity carries, which a copy and a new tab take. */
function activityHref(): string | null {
  return screen.getByRole("link", { name: "Activity" }).getAttribute("href");
}

/** A move the page makes to `to`, as a view's control or a key makes it. */
function move(to: string, options?: NavigateOptions): void {
  const navigate = held.navigate;
  if (navigate === undefined) throw new Error("the probe was not drawn");
  act(() => navigate(to, options));
}

test("from an issue's address with ?issue_page=new, every move to another of its addresses keeps it, and Back finds it", () => {
  const view = drawAt("/issues/CORE-1?issue_page=new");
  try {
    move(ACTIVITY);
    expect(address()).toBe("/issues/CORE-1/activity?issue_page=new");
    move("/issues/CORE-1/collaborators", { replace: true });
    expect(address()).toBe("/issues/CORE-1/collaborators?issue_page=new");
    act(() => held.navigate?.(-1));
    expect(address()).toBe("/issues/CORE-1?issue_page=new");
  } finally {
    view.unmount();
  }
});

test("a move to a thread's address keeps its own parameters and hash and adds the parameter after them", () => {
  const view = drawAt("/issues/CORE-1?issue_page=new");
  try {
    move("/issues/CORE-1/comments/abc?mark=x#b-1");
    expect(address()).toBe("/issues/CORE-1/comments/abc?mark=x&issue_page=new#b-1");
  } finally {
    view.unmount();
  }
});

test("a move to another issue adds nothing, and the links drawn there carry nothing", () => {
  const view = drawAt("/issues/CORE-1?issue_page=new");
  try {
    expect(activityHref()).toBe(`${ACTIVITY}?issue_page=new`);
    move("/issues/CORE-2");
    expect(address()).toBe("/issues/CORE-2");
    expect(activityHref()).toBe(ACTIVITY);
  } finally {
    view.unmount();
  }
});

test("a move to a page that is no issue's adds nothing", () => {
  const view = drawAt("/issues/CORE-1?issue_page=new");
  try {
    expect(activityHref()).toBe(`${ACTIVITY}?issue_page=new`);
    move("/inbox");
    expect(address()).toBe("/inbox");
    expect(activityHref()).toBe(ACTIVITY);
  } finally {
    view.unmount();
  }
});

test("a target that names issue_page itself is left as it names it", () => {
  const view = drawAt("/issues/CORE-1?issue_page=new");
  try {
    expect(activityHref()).toBe(`${ACTIVITY}?issue_page=new`);
    move(`${ACTIVITY}?issue_page=new`);
    expect(address()).toBe(`${ACTIVITY}?issue_page=new`);
    move("/issues/CORE-1?issue_page=old");
    expect(address()).toBe("/issues/CORE-1?issue_page=old");
    expect(activityHref()).toBe(ACTIVITY);
  } finally {
    view.unmount();
  }
});

/** Addresses of the issue the parameter does not open the new page at. */
const UNOPENED = [
  "/issues/CORE-1",
  "/issues/CORE-1?issue_page=old",
  "/issues/CORE-1?issue_page=",
  "/issues/CORE-1?issue_page=new2",
] as const;

for (const from of UNOPENED) {
  test(`from ${from}, nothing is added until an address carries ?issue_page=new`, () => {
    const view = drawAt(from);
    try {
      expect(activityHref()).toBe(ACTIVITY);
      move(ACTIVITY);
      expect(address()).toBe(ACTIVITY);
      move("/issues/CORE-1?issue_page=new");
      expect(activityHref()).toBe(`${ACTIVITY}?issue_page=new`);
    } finally {
      view.unmount();
    }
  });
}

test("the href of a link drawn at the address carries the parameter, and following the link keeps it", async () => {
  const user = userEvent.setup();
  const view = drawAt("/issues/CORE-1?issue_page=new");
  try {
    expect(activityHref()).toBe(`${ACTIVITY}?issue_page=new`);
    await user.click(screen.getByRole("link", { name: "Activity" }));
    expect(address()).toBe(`${ACTIVITY}?issue_page=new`);
  } finally {
    view.unmount();
  }
});

// A link to the address the reader is at replaces that entry, as it does with no parameter: the
// router compares the link's target, which lacks the parameter, with the address, which carries
// it, and pushes, and the parameter added made that push an entry the same as the one the press
// was made on, so Back stayed where the reader was. The rule is the router's own for a link, which
// counts an empty `#` as nothing: `navigate` drops one from its target, the navigator does not.
test("a link to the address the reader is at adds no history entry, nor does a push there whose target ends in an empty #, where a link to another of the issue's addresses adds one", async () => {
  const user = userEvent.setup();
  const view = drawAt("/issues/CORE-1?issue_page=new");
  try {
    await user.click(screen.getByRole("link", { name: "Activity" }));
    expect(address()).toBe(`${ACTIVITY}?issue_page=new`);
    act(() => held.navigate?.(-1));
    expect(address(), "the link to another of the issue's addresses added no history entry").toBe(
      "/issues/CORE-1?issue_page=new"
    );
    act(() => held.navigate?.(1));
    expect(address()).toBe(`${ACTIVITY}?issue_page=new`);

    await user.click(screen.getByRole("link", { name: "Activity" }));
    expect(address()).toBe(`${ACTIVITY}?issue_page=new`);
    act(() => held.navigate?.(-1));
    expect(
      address(),
      "the link to the address the reader is at added a history entry, and Back stayed there"
    ).toBe("/issues/CORE-1?issue_page=new");

    const { navigator } = held;
    if (navigator === undefined) throw new Error("the probe was not drawn");
    act(() => held.navigate?.(1));
    act(() => navigator.push(`${ACTIVITY}#`));
    act(() => held.navigate?.(-1));
    expect(
      address(),
      "a push to the address the reader is at, its target ending in an empty #, added a history entry"
    ).toBe("/issues/CORE-1?issue_page=new");
  } finally {
    view.unmount();
  }
});
