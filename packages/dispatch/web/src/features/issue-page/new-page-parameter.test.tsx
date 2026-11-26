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

/** Draws the probe under the rule, with `words` beside it, in a router whose entries are
 *  `before` and then `path`, at `path`. */
function drawAt(path: string, words?: ReactNode, before: readonly string[] = []): RenderResult {
  return render(
    <MemoryRouter initialEntries={[...before, path]}>
      <KeepNewIssuePage>
        <Probe />
        {words}
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

/** The address the cases below open the new page at. */
const OPENED = "/issues/CORE-1?issue_page=new";

/**
 * Dispatches a click of `init` (a plain one by default) on the link named `name`, and answers
 * whether it was left to the browser: whether no handler, the rule's among them, prevented it. A
 * listener added after the rule's then prevents it, since the unit suite's document loads no
 * page.
 */
function press(name: string, init: MouseEventInit = {}): boolean {
  const link = screen.getByRole("link", { name });
  let left = false;
  const stop = (event: Event) => {
    left = !event.defaultPrevented;
    event.preventDefault();
  };
  document.addEventListener("click", stop);
  try {
    act(() => {
      link.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, ...init }));
    });
  } finally {
    document.removeEventListener("click", stop);
  }
  return left;
}

// A rendered body's link is a plain `<a href>` the router never drew: its `href` lacks the
// parameter, and the browser would load the issue's address without it, where a reader whose
// setting is off gets today's page. A plain click on one to another of the issue's addresses is
// followed in place with the parameter added, and Back finds the address the click was made at.
test("a plain click on a rendered link to another of the issue's addresses is followed in place with the parameter, and Back finds the address it was made at", () => {
  const view = drawAt(OPENED, <a href={ACTIVITY}>Words</a>);
  try {
    expect(press("Words"), "the click was left to load the page").toBe(false);
    expect(address()).toBe(`${ACTIVITY}?issue_page=new`);
    act(() => held.navigate?.(-1));
    expect(address()).toBe(OPENED);
  } finally {
    view.unmount();
  }
});

test("a plain click on a rendered link to the address the reader is at adds no history entry", () => {
  const view = drawAt(OPENED, <a href="/issues/CORE-1">Words</a>, ["/inbox"]);
  try {
    expect(press("Words"), "the click was left to load the page").toBe(false);
    expect(address()).toBe(OPENED);
    act(() => held.navigate?.(-1));
    expect(address(), "the click added a history entry, and Back stayed where it was made").toBe(
      "/inbox"
    );
  } finally {
    view.unmount();
  }
});

test("a plain click on a rendered link to a thread keeps its parameters and hash and adds the parameter after them", () => {
  const view = drawAt(OPENED, <a href="/issues/CORE-1/comments/abc?mark=x#b-1">Words</a>);
  try {
    expect(press("Words"), "the click was left to load the page").toBe(false);
    expect(address()).toBe("/issues/CORE-1/comments/abc?mark=x&issue_page=new#b-1");
  } finally {
    view.unmount();
  }
});

/** The clicks on a rendered link the rule leaves to the browser at `OPENED`: each by what it is,
 *  with the link's target (the activity's by default), its own attributes and the click's keys
 *  and button. */
const LEFT_TO_THE_BROWSER: readonly {
  readonly click?: MouseEventInit;
  readonly href?: string;
  readonly link?: { readonly download?: string; readonly target?: string };
  readonly what: string;
}[] = [
  { click: { ctrlKey: true }, what: "a click with Control held" },
  { click: { metaKey: true }, what: "a click with Command held" },
  { click: { shiftKey: true }, what: "a click with Shift held" },
  { click: { altKey: true }, what: "a click with Alt held" },
  { click: { button: 1 }, what: "a click of the middle button" },
  { link: { target: "_blank" }, what: "a link to another window" },
  { link: { download: "" }, what: "a link to a download" },
  { href: "http://elsewhere.test/issues/CORE-1/activity", what: "a link to another origin" },
  { href: "/issues/CORE-2", what: "a link to another issue" },
  { href: "/issues/CORE-1/activity?issue_page=old", what: "a link that names issue_page itself" },
];

for (const { click, href = ACTIVITY, link, what } of LEFT_TO_THE_BROWSER) {
  test(`${what} is left to the browser, and the router stays where it is`, () => {
    const view = drawAt(
      OPENED,
      <a href={href} {...link}>
        Words
      </a>
    );
    try {
      expect(press("Words", click), `${what} was followed in place`).toBe(true);
      expect(address()).toBe(OPENED);
    } finally {
      view.unmount();
    }
  });
}

// A link in an editable text is the editor's: a plain click puts the caret in it, which the
// document editor does on purpose, and the page must not move there.
test("a link inside an editable text is left to the editor, and the router stays where it is", () => {
  const view = drawAt(
    OPENED,
    // biome-ignore lint/a11y/useSemanticElements: the case draws the attribute the editor's text carries.
    <div contentEditable role="textbox" suppressContentEditableWarning tabIndex={0}>
      <a href={ACTIVITY}>Words</a>
    </div>
  );
  try {
    expect(press("Words"), "a click on a link in an editable text was followed in place").toBe(
      true
    );
    expect(address()).toBe(OPENED);
  } finally {
    view.unmount();
  }
});

/** A rendered link whose own handler takes a plain click, as a row's words take theirs: it moves
 *  to the issue's collaborators itself, wherever its `href` points. */
function TakenLink(): ReactNode {
  const navigate = useNavigate();
  return (
    <a
      href={ACTIVITY}
      onClick={(event) => {
        event.preventDefault();
        navigate("/issues/CORE-1/collaborators");
      }}
    >
      Taken
    </a>
  );
}

// A click a handler of the page took is prevented before it reaches the document, and the rule
// leaves it to that handler: the router moves once, where the handler moved it.
test("a click a link's own handler already took moves the router once, where that handler moved it", () => {
  const view = drawAt(OPENED, <TakenLink />);
  try {
    expect(press("Taken"), "the link's own handler left the click to load the page").toBe(false);
    expect(address(), "the rule moved the router after the link's own handler").toBe(
      "/issues/CORE-1/collaborators?issue_page=new"
    );
    act(() => held.navigate?.(-1));
    expect(address(), "the click moved the router twice").toBe(OPENED);
  } finally {
    view.unmount();
  }
});

test("a click the router's Link already took moves the router once, where the Link moved it", () => {
  const view = drawAt(OPENED);
  try {
    expect(press("Activity"), "the router's Link left the click to load the page").toBe(false);
    expect(address()).toBe(`${ACTIVITY}?issue_page=new`);
    act(() => held.navigate?.(-1));
    expect(address(), "the click moved the router twice").toBe(OPENED);
  } finally {
    view.unmount();
  }
});

for (const from of UNOPENED) {
  test(`from ${from}, a plain click on a rendered link to the issue is left to the browser`, () => {
    const view = drawAt(from, <a href={ACTIVITY}>Words</a>);
    try {
      expect(press("Words"), "the click was followed in place").toBe(true);
      expect(address()).toBe(from);
    } finally {
      view.unmount();
    }
  });
}

for (const away of ["/issues/CORE-2", "/inbox"]) {
  test(`once the reader moves to ${away}, a plain click on a rendered link to the first issue is left to the browser`, () => {
    const view = drawAt(OPENED, <a href={ACTIVITY}>Words</a>);
    try {
      move(away);
      expect(address()).toBe(away);
      expect(press("Words"), "the rule still followed the issue's links once the reader left").toBe(
        true
      );
      expect(address()).toBe(away);
    } finally {
      view.unmount();
    }
  });
}
