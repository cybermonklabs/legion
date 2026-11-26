import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import type { OpenLink } from "@legion/proof-editor";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, within } from "@testing-library/react";
import { type ReactNode, useLayoutEffect, useRef } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import type { IssueReferenceSummary } from "../../api/types";
import { LinkClickHost, linkDestination, useLinkFollower, useLinkOpener } from "./link-open";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";

/** The page the reader is on, which a relative target is read against. */
const PAGE = "http://dispatch.test/issues/CORE-9";

let windowOpen: Mock<typeof window.open>;
let locationAssign: Mock<typeof window.location.assign>;

beforeEach(() => {
  windowOpen = spyOn(window, "open").mockImplementation(() => null);
  locationAssign = spyOn(window.location, "assign").mockImplementation(() => {});
});

afterEach(() => {
  windowOpen.mockRestore();
  locationAssign.mockRestore();
});

test("each target goes where the one rule says", () => {
  expect(linkDestination("dispatch://CORE-1/spec", PAGE)).toMatchObject({
    kind: "reference",
    route: { key: "CORE-1", kind: "spec" },
  });
  expect(linkDestination("/issues/CORE-1/spec?v=2#b-x", PAGE)).toEqual({
    href: "http://dispatch.test/issues/CORE-1/spec?v=2#b-x",
    kind: "page",
    path: "/issues/CORE-1/spec?v=2#b-x",
  });
  expect(linkDestination("http://dispatch.test/projects/CORE", PAGE)).toEqual({
    href: "http://dispatch.test/projects/CORE",
    kind: "page",
    path: "/projects/CORE",
  });
  // A link to a block of the same document is its page at the block.
  expect(linkDestination("#b-x", PAGE)).toEqual({
    href: "http://dispatch.test/issues/CORE-9#b-x",
    kind: "page",
    path: "/issues/CORE-9#b-x",
  });
  for (const path of [
    "/api/v1/issues/CORE-1/artifacts/shot-png/versions/1",
    "/auth/start",
    "/ws/live",
  ]) {
    expect(linkDestination(path, PAGE)).toEqual({
      href: `http://dispatch.test${path}`,
      kind: "server",
    });
  }
  for (const target of ["https://example.com/a", "mailto:a@b.test", "tel:+15550100"]) {
    expect(linkDestination(target, PAGE)).toEqual({ href: target, kind: "away" });
  }
  // A scheme no link of Dispatch opens, and a reference that names nothing, go nowhere.
  for (const target of [
    "javascript:alert(1)",
    "data:text/html,x",
    "ftp://example.com/f",
    "dispatch://",
  ]) {
    expect({ destination: linkDestination(target, PAGE), target }).toEqual({
      destination: undefined,
      target,
    });
  }
});

/** The opener the page holds, at `PAGE`'s route, beside the route it is on. */
function renderOpener(queryClient = new QueryClient()) {
  let opener: OpenLink | undefined;
  function Probe(): ReactNode {
    opener = useLinkOpener();
    const location = useLocation();
    return <p data-testid="route">{`${location.pathname}${location.search}${location.hash}`}</p>;
  }
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/issues/CORE-9"]}>
        <Probe />
      </MemoryRouter>
    </QueryClientProvider>
  );
  return {
    open: async (target: string, newTab = false) => {
      await act(async () => {
        opener?.(target, { newTab });
      });
    },
    route: () => within(view.container).getByTestId("route").textContent,
    view,
  };
}

test("a reference and a page of Dispatch open in the same tab, through the router", async () => {
  const { open, route, view } = renderOpener();
  try {
    await open("dispatch://CORE-1/spec");
    expect(route()).toBe("/issues/CORE-1/spec");
    await open("/issues/CORE-2#b-x");
    expect(route()).toBe("/issues/CORE-2#b-x");
    expect(windowOpen).not.toHaveBeenCalled();
    expect(locationAssign).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("a block of a spec pinned to a version opens that version under the spec's own slug", async () => {
  const queryClient = new QueryClient();
  const summary: IssueReferenceSummary = {
    documents: [
      {
        id: "a-1",
        kind: "markdown",
        latest_version: 3,
        name: "spec.md",
        primary: true,
        slug: "spec-md",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "todo",
    title: "Ship the release",
  };
  queryClient.setQueryData(["issue-summary", "CORE-1"], summary);
  const { open, route, view } = renderOpener(queryClient);
  try {
    await open("dispatch://CORE-1/spec#b-x@v2");
    expect(route()).toBe("/issues/CORE-1/artifacts/spec-md?v=2");
  } finally {
    view.unmount();
  }
});

test("another site, a mail address and a phone number open in a new tab with no handle on this page", async () => {
  const { open, route, view } = renderOpener();
  try {
    for (const target of ["https://example.com/a", "mailto:a@b.test", "tel:+15550100"]) {
      await open(target);
      expect(windowOpen).toHaveBeenLastCalledWith(target, "_blank", "noopener,noreferrer");
    }
    expect(route()).toBe("/issues/CORE-9");
  } finally {
    view.unmount();
  }
});

test("a Ctrl, Cmd or middle click opens a page of Dispatch in a new tab", async () => {
  const { open, route, view } = renderOpener();
  try {
    await open("dispatch://CORE-1", true);
    expect(windowOpen).toHaveBeenLastCalledWith("/issues/CORE-1", "_blank", "noopener");
    await open("/issues/CORE-2#b-x", true);
    expect(windowOpen).toHaveBeenLastCalledWith(
      "http://dispatch.test/issues/CORE-2#b-x",
      "_blank",
      "noopener"
    );
    expect(route()).toBe("/issues/CORE-9");
  } finally {
    view.unmount();
  }
});

// Nothing shows a person (R5), so a reference to one opens no page: the press the editor or the
// shell's host took goes nowhere, in this tab or a new one, and the reader stays where they were.
test("a reference to a person opens nothing, in the same tab or a new one", async () => {
  const { open, route, view } = renderOpener();
  try {
    await open("dispatch://person/alice@example.com");
    expect(route()).toBe("/issues/CORE-9");
    await open("dispatch://person/alice@example.com", true);
    expect(windowOpen).not.toHaveBeenCalled();
    expect(route()).toBe("/issues/CORE-9");
  } finally {
    view.unmount();
  }
});

/** The follower a key holds (`useLinkFollower`), at `PAGE`'s route, beside the route it is on,
 *  and `follow`, which follows an element holding a link to `target`, or no link at all. */
function renderFollower() {
  let follower: ((element: Element | null) => void) | undefined;
  function Probe(): ReactNode {
    follower = useLinkFollower();
    const location = useLocation();
    return <p data-testid="route">{`${location.pathname}${location.search}${location.hash}`}</p>;
  }
  const view = render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={["/issues/CORE-9"]}>
        <Probe />
      </MemoryRouter>
    </QueryClientProvider>
  );
  return {
    follow: async (target: string | null) => {
      const host = document.createElement("span");
      if (target !== null) {
        const link = document.createElement("a");
        link.href = target;
        host.append(link);
      }
      await act(async () => {
        follower?.(host);
      });
    },
    route: () => within(view.container).getByTestId("route").textContent,
    view,
  };
}

// `o` on a thread's short row follows its title in this tab, as a click on a link of the page:
// a page of Dispatch through the router, with no new load of the application; any other address
// by the browser's own load, in this tab, since the key asked for this one; nothing where the
// element holds no link.
test("a key's follow of a link opens a page of Dispatch through the router and loads any other address in this tab", async () => {
  const { follow, route, view } = renderFollower();
  try {
    await follow(null);
    expect(route()).toBe("/issues/CORE-9");
    await follow("/issues/CORE-1/comments/c9");
    expect(route()).toBe("/issues/CORE-1/comments/c9");
    expect(locationAssign).not.toHaveBeenCalled();
    await follow("https://example.com/a");
    expect(locationAssign).toHaveBeenLastCalledWith("https://example.com/a");
    await follow("/api/v1/issues/CORE-1");
    expect(locationAssign).toHaveBeenLastCalledWith("http://dispatch.test/api/v1/issues/CORE-1");
    expect(locationAssign).toHaveBeenCalledTimes(2);
    expect(windowOpen).not.toHaveBeenCalled();
    expect(route()).toBe("/issues/CORE-1/comments/c9");
  } finally {
    view.unmount();
  }
});

/** Targets on Dispatch whose path starts with `//`: a `.` or `..` segment before the slashes
 *  (`%2e` is a `.` too), this origin before them, or a backslash among them, which a web address
 *  reads as a slash. */
const DOUBLE_SLASH_PATHS = [
  "/.//evil.example/x",
  "http://dispatch.test//evil.example/x",
  "/x/..//evil.example/x",
  "/%2e//evil.example/x",
  "/./\\evil.example/x",
] as const;

// A target that names another host after slashes or backslashes (`//evil.example/x`,
// `/\evil.example/x`, `\\evil.example/x`) is another site; one whose path on Dispatch starts with
// `//` is a page of Dispatch, kept whole as well as by its path.
test("a target whose path starts with // is another site only where it names another host", () => {
  for (const target of [
    "//evil.example/x",
    "/\\evil.example/x",
    "\\\\evil.example/x",
    "\\/evil.example/x",
    "///evil.example/x",
  ]) {
    expect({ destination: linkDestination(target, PAGE), target }).toEqual({
      destination: { href: "http://evil.example/x", kind: "away" },
      target,
    });
  }
  for (const target of DOUBLE_SLASH_PATHS) {
    expect({ destination: linkDestination(target, PAGE), target }).toEqual({
      destination: {
        href: "http://dispatch.test//evil.example/x",
        kind: "page",
        path: "//evil.example/x",
      },
      target,
    });
  }
});

// A document's author can write `[spec](/.//evil.example/login)`: the sanitizer keeps it, the
// status bar shows a Dispatch address, and the path alone, `//evil.example/login`, is the address
// `http://evil.example/login` to `window.open`. A new tab opens the page's whole address instead.
test("a new tab of a page of Dispatch stays on Dispatch, whatever run of slashes starts its path", async () => {
  const { open, route, view } = renderOpener();
  try {
    for (const target of DOUBLE_SLASH_PATHS) {
      await open(target, true);
      expect({ opened: windowOpen.mock.lastCall, target }).toEqual({
        opened: ["http://dispatch.test//evil.example/x", "_blank", "noopener"],
        target,
      });
    }
    // In place, the router reads the path as one on this origin.
    await open("/.//evil.example/x");
    expect(route()).toBe("/evil.example/x");
    expect(locationAssign).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("an address the server answers itself is loaded by the browser, and other schemes open nothing", async () => {
  const { open, route, view } = renderOpener();
  const bytes = "http://dispatch.test/api/v1/issues/CORE-1/artifacts/shot-png/versions/1";
  try {
    await open("/api/v1/issues/CORE-1/artifacts/shot-png/versions/1");
    expect(locationAssign).toHaveBeenCalledWith(bytes);
    await open("/api/v1/issues/CORE-1/artifacts/shot-png/versions/1", true);
    expect(windowOpen).toHaveBeenLastCalledWith(bytes, "_blank", "noopener");
    windowOpen.mockClear();
    locationAssign.mockClear();

    await open("javascript:alert(1)");
    await open("dispatch://");
    expect(windowOpen).not.toHaveBeenCalled();
    expect(locationAssign).not.toHaveBeenCalled();
    expect(route()).toBe("/issues/CORE-9");
  } finally {
    view.unmount();
  }
});

/** Whether each click that reached the window had been prevented by then - by the host, which
 *  listens on the document below it - and the click prevented after that, so happy-dom follows no
 *  link the host left to the browser. */
function recordPrevented(): { readonly prevented: boolean[]; readonly stop: () => void } {
  const prevented: boolean[] = [];
  const listener = (event: Event) => {
    prevented.push(event.defaultPrevented);
    event.preventDefault();
  };
  window.addEventListener("click", listener);
  return { prevented, stop: () => window.removeEventListener("click", listener) };
}

function click(target: Element, init: MouseEventInit = {}): void {
  act(() => {
    target.dispatchEvent(
      new MouseEvent("click", { bubbles: true, button: 0, cancelable: true, ...init })
    );
  });
}

function renderBodies(body: ReactNode) {
  function Route(): ReactNode {
    return <p data-testid="route">{useLocation().pathname}</p>;
  }
  const view = render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={["/issues/CORE-9"]}>
        <LinkClickHost />
        <Route />
        {body}
      </MemoryRouter>
    </QueryClientProvider>
  );
  return {
    link: (name: string) => within(view.container).getByRole("link", { name }),
    route: () => within(view.container).getByTestId("route").textContent,
    view,
  };
}

test("a plain click on a link in a rendered body opens it by the one rule", () => {
  const clicks = recordPrevented();
  const { link, route, view } = renderBodies(
    <div className="dispatch-markdown">
      <a href="/issues/CORE-1">a page</a>
      <a href="https://example.com/a">elsewhere</a>
      <a data-dispatch-ref="dispatch://CORE-2" href="/issues/CORE-2">
        a reference
      </a>
    </div>
  );
  try {
    click(link("a page"));
    expect(route()).toBe("/issues/CORE-1");
    click(link("elsewhere"));
    expect(windowOpen).toHaveBeenLastCalledWith(
      "https://example.com/a",
      "_blank",
      "noopener,noreferrer"
    );
    // A reference link opens what it names, which its `href` only follows.
    click(link("a reference"));
    expect(route()).toBe("/issues/CORE-2");
    expect(clicks.prevented).toEqual([true, true, true]);
  } finally {
    clicks.stop();
    view.unmount();
  }
});

// The host listens from the commit that first draws it, as the keymap does: a click made before
// that commit's passive effects run reached no listener while the host attached it in one, and
// the browser followed the link with a new load of the application. Here the click comes from a
// layout effect of the body drawn beside the host, which runs in that commit after the host's own
// and before any passive effect of it. The router is drawn a commit before them, as the
// application's is before the shell, so it listens to its history by then.
test("a click made in the commit that first draws the host opens its link by the one rule", () => {
  const clicks = recordPrevented();
  function ClickedAsDrawn(): ReactNode {
    const link = useRef<HTMLAnchorElement>(null);
    useLayoutEffect(() => {
      link.current?.dispatchEvent(
        new MouseEvent("click", { bubbles: true, button: 0, cancelable: true })
      );
    }, []);
    return (
      <div className="dispatch-markdown">
        <a href="/issues/CORE-1" ref={link}>
          a page
        </a>
      </div>
    );
  }
  function Route(): ReactNode {
    return <p data-testid="route">{useLocation().pathname}</p>;
  }
  const queryClient = new QueryClient();
  const page = (drawn: boolean) => (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/issues/CORE-9"]}>
        <Route />
        {drawn ? (
          <>
            <LinkClickHost />
            <ClickedAsDrawn />
          </>
        ) : null}
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(page(false));
  try {
    view.rerender(page(true));
    expect(clicks.prevented).toEqual([true]);
    expect(within(view.container).getByTestId("route").textContent).toBe("/issues/CORE-1");
  } finally {
    clicks.stop();
    view.unmount();
  }
});

test("a click the host leaves alone: a modifier, a middle button, one taken already, a server address, a link outside a body", () => {
  const clicks = recordPrevented();
  const { link, route, view } = renderBodies(
    <>
      <div className="dispatch-markdown">
        <a href="/issues/CORE-1">a page</a>
        <a href="/api/v1/issues/CORE-1/artifacts/notes-md/versions/1">the bytes</a>
        {/* A link whose click a handler takes first, as a picture takes it for its lightbox. */}
        <a href="/issues/CORE-3" onClick={(event) => event.preventDefault()}>
          taken
        </a>
      </div>
      <a href="/issues/CORE-4">outside</a>
    </>
  );
  try {
    click(link("a page"), { ctrlKey: true });
    click(link("a page"), { metaKey: true });
    click(link("a page"), { shiftKey: true });
    click(link("a page"), { button: 1 });
    click(link("the bytes"));
    click(link("outside"));
    expect(clicks.prevented).toEqual([false, false, false, false, false, false]);
    click(link("taken"));
    expect(route()).toBe("/issues/CORE-9");
    expect(windowOpen).not.toHaveBeenCalled();
  } finally {
    clicks.stop();
    view.unmount();
  }
});

// The sanitizer draws a link whose scheme it refuses (`javascript:`, `data:`) with an empty `href`
// (`linkAddress`), which the browser reads as the page the reader is on: an address the link never
// named. No click opens it, plain or Ctrl, nor a middle click, which a browser sends as `auxclick`.
test("a link with a blank href opens nothing, on a plain click, a Ctrl click or a middle click", () => {
  const clicks = recordPrevented();
  // Drawn as the sanitizer draws them: Biome's anchor rule refuses an empty `href` written in JSX.
  function SanitizedBody(): ReactNode {
    const body = useRef<HTMLDivElement>(null);
    useLayoutEffect(() => {
      if (body.current !== null) body.current.innerHTML = '<a href="">x</a><a href="  ">y</a>';
    }, []);
    return <div className="dispatch-markdown" ref={body} />;
  }
  const { route, view } = renderBodies(<SanitizedBody />);
  try {
    // By its words: a link whose `href` is empty has no `link` role to find it by.
    const middles = ["x", "y"].map((name) => {
      const link = within(view.container).getByText(name);
      click(link);
      click(link, { ctrlKey: true });
      const middle = new MouseEvent("auxclick", { bubbles: true, button: 1, cancelable: true });
      act(() => {
        link.dispatchEvent(middle);
      });
      return middle.defaultPrevented;
    });
    expect(clicks.prevented).toEqual([true, true, true, true]);
    expect(middles).toEqual([true, true]);
    expect(route()).toBe("/issues/CORE-9");
    expect(windowOpen).not.toHaveBeenCalled();
    expect(locationAssign).not.toHaveBeenCalled();
  } finally {
    clicks.stop();
    view.unmount();
  }
});

test("a link in a Markdown body opens through the host", async () => {
  await warmMarkdownRenderer();
  const clicks = recordPrevented();
  const { view } = renderBodies(
    <MarkdownBody markdown="See https://example.com/docs for details" />
  );
  try {
    const link = await within(view.container).findByRole("link", {
      name: "https://example.com/docs",
    });
    click(link);
    expect(windowOpen).toHaveBeenLastCalledWith(
      "https://example.com/docs",
      "_blank",
      "noopener,noreferrer"
    );
    expect(clicks.prevented).toEqual([true]);
  } finally {
    clicks.stop();
    view.unmount();
  }
});
