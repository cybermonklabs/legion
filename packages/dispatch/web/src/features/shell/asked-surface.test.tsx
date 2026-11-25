import { expect, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the text the box holds is set with one change event, since `user.keyboard` into that uncontrolled box makes React print its unawaited-act message in this environment; the keys a browser types into it are `e2e/keyboard-issue.e2e.ts`'s
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useEffect, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { quietConsoleError } from "../../__tests__/act-warnings";
import { COMPOSITION_MARKS } from "../../__tests__/composition-marks";
import { failingChunkComponent } from "../../__tests__/failing-chunk";
import { type AskedSurfaceProps, askedSurface } from "./asked-surface";
import { type Chunk, chunk } from "./chunk-component";
import { chunkLoadsOut, onlyPrefetchesOut } from "./chunk-loads";

/** A surface as the bar and the peek are: mounted while closed, drawing nothing then. */
function Surface({ open, typed = "" }: AskedSurfaceProps): ReactNode {
  return open === false ? null : <p>Surface with {JSON.stringify(typed)}</p>;
}

/** The name the box and the alert take from the surface they stand for. */
const LOADING = "Example (loading)";

/** The shell's part: an opener, and the surface it opens. */
function Shell({ source }: { source: Chunk<AskedSurfaceProps> }): ReactNode {
  const [Asked] = useState(() => askedSurface(source, "Example"));
  const [open, setOpen] = useState(false);
  return (
    <MemoryRouter>
      <button onClick={() => setOpen(true)} type="button">
        Open
      </button>
      <Asked onClose={() => setOpen(false)} open={open} />
    </MemoryRouter>
  );
}

// A surface whose chunk is in opens in the commit of the press that asked for it, so the next key
// the reader types meets the surface and not the page.
test("a surface whose chunk is in draws in the commit of the press", async () => {
  const user = userEvent.setup();
  const source = chunk(async () => Surface);
  await source.load();
  const seen: string[] = [];
  function Watch({ open }: { open: boolean }): ReactNode {
    useEffect(() => {
      if (open) seen.push(document.body.textContent ?? "");
    }, [open]);
    return null;
  }
  const Asked = askedSurface(source, "Example");
  function Opener(): ReactNode {
    const [open, setOpen] = useState(false);
    return (
      <MemoryRouter>
        <button onClick={() => setOpen(true)} type="button">
          Open
        </button>
        <Asked onClose={() => setOpen(false)} open={open} />
        <Watch open={open} />
      </MemoryRouter>
    );
  }
  const view = render(<Opener />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(seen).toEqual(['OpenSurface with ""']);
    expect(screen.queryByRole("textbox", { name: LOADING })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// While the chunk is out, the box holds the focus, and what the reader types there is the
// surface's when it lands. The box is named for the surface it stands for, and is busy.
test("while the chunk is out a box holds the focus, and what is typed into it reaches the surface", async () => {
  const user = userEvent.setup();
  const { promise, resolve } = Promise.withResolvers<typeof Surface>();
  const view = render(<Shell source={chunk(() => promise)} />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    const box = screen.getByRole("textbox", { name: LOADING });
    expect(box).toHaveFocus();
    expect(box).toHaveAttribute("aria-busy", "true");
    // What the box holds as the chunk lands is what this row is about; the keys a browser types
    // into it are `e2e/keyboard-issue.e2e.ts`'s.
    fireEvent.change(box, { target: { value: "1e" } });
    await act(async () => resolve(Surface));
    expect(screen.getByText('Surface with "1e"')).toBeDefined();
    expect(screen.queryByRole("textbox", { name: LOADING })).not.toBeInTheDocument();
  } finally {
    resolve(Surface);
    view.unmount();
  }
});

// Escape calls the open off: the focus goes back to the opener, and a chunk that lands after that
// opens nothing.
test("Escape in the box calls the open off, and the chunk that lands after opens nothing", async () => {
  const user = userEvent.setup();
  const { promise, resolve } = Promise.withResolvers<typeof Surface>();
  const view = render(<Shell source={chunk(() => promise)} />);
  try {
    const opener = screen.getByRole("button", { name: "Open" });
    await user.click(opener);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("textbox", { name: LOADING })).not.toBeInTheDocument();
    expect(opener).toHaveFocus();
    await act(async () => resolve(Surface));
    expect(screen.queryByText(/Surface with/)).not.toBeInTheDocument();
    expect(opener).toHaveFocus();
  } finally {
    resolve(Surface);
    view.unmount();
  }
});

// Tab and Shift+Tab stay in the box, as they stay in an open dialog. Let through, either would
// take the focus out of the box: its blur would call the open off, and the next key would reach
// the page under it.
test("Tab and Shift+Tab leave the focus in the box, and the open stands", async () => {
  const user = userEvent.setup();
  const { promise, resolve } = Promise.withResolvers<typeof Surface>();
  const view = render(
    <>
      <Shell source={chunk(() => promise)} />
      <input aria-label="After" />
    </>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    const box = screen.getByRole("textbox", { name: LOADING });
    await user.tab({ shift: true });
    expect(box, "Shift+Tab took the focus out of the box").toHaveFocus();
    await user.tab();
    expect(box, "Tab took the focus out of the box").toHaveFocus();
    // fireEvent: the box's text is set with one change event, since `user.keyboard` into that
    // uncontrolled box makes React print its unawaited-act message here; a browser's keys into it
    // are `e2e/keyboard-issue.e2e.ts`'s.
    fireEvent.change(box, { target: { value: "1" } });
    await act(async () => resolve(Surface));
    expect(screen.getByText('Surface with "1"')).toBeDefined();
  } finally {
    resolve(Surface);
    view.unmount();
  }
});

// An Escape that ends an input method's composition only ends it, as it does in the open
// surface: the open stands, and what the reader typed stays. Chromium marks that Escape
// `isComposing`; Safari sends it after its compositionend with `isComposing` false and keyCode
// 229 (WebKit bug 165004).
test.each(
  COMPOSITION_MARKS
)("an Escape that ends a composition, marked by %s, leaves the open standing with what was typed", async (_mark, marked) => {
  const user = userEvent.setup();
  const { promise, resolve } = Promise.withResolvers<typeof Surface>();
  const view = render(<Shell source={chunk(() => promise)} />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    const box = screen.getByRole("textbox", { name: LOADING });
    // fireEvent: an input method writes its text with no key of its own, so the text arrives as one
    // change event and the composition's Escape as the event itself; user-event types no
    // composition.
    fireEvent.change(box, { target: { value: "日本" } });
    fireEvent.keyDown(box, { ...marked, key: "Escape" });
    expect(box).toHaveFocus();
    await act(async () => resolve(Surface));
    expect(screen.getByText('Surface with "日本"')).toBeDefined();
  } finally {
    resolve(Surface);
    view.unmount();
  }
});

// A press elsewhere takes the focus out of the box, and that calls the open off too.
test("the focus leaving the box calls the open off", async () => {
  const user = userEvent.setup();
  const { promise, resolve } = Promise.withResolvers<typeof Surface>();
  const view = render(
    <>
      <Shell source={chunk(() => promise)} />
      <input aria-label="Elsewhere" />
    </>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    const elsewhere = screen.getByRole("textbox", { name: "Elsewhere" });
    await user.click(elsewhere);
    expect(screen.queryByRole("textbox", { name: LOADING })).not.toBeInTheDocument();
    await act(async () => resolve(Surface));
    expect(screen.queryByText(/Surface with/)).not.toBeInTheDocument();
    expect(elsewhere).toHaveFocus();
  } finally {
    resolve(Surface);
    view.unmount();
  }
});

// A download that fails online closes the open, so the next press asks for the chunk again
// rather than finding the surface open with nothing drawn. It draws no alert: it is no fault of
// the surface's render.
test("a chunk that fails to download closes the open, and the next press asks for it again", async () => {
  const user = userEvent.setup();
  let imports = 0;
  const source = chunk(async () => {
    imports += 1;
    if (imports === 1) throw new TypeError("Failed to fetch dynamically imported module");
    return Surface;
  });
  const view = render(<Shell source={source} />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    await act(async () => undefined);
    expect(screen.queryByRole("textbox", { name: LOADING })).not.toBeInTheDocument();
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(await screen.findByText('Surface with ""')).toBeDefined();
    expect(imports).toBe(2);
  } finally {
    view.unmount();
  }
});

// A render under the surface that throws - a chunk the surface draws from failing, or any other
// fault - draws an alert in the surface's place and leaves the shell drawn: the reader is told,
// rather than finding the surface closed with no word. Its Close closes the surface and gives the
// focus back to the opener, and the next open draws the surface afresh.
test("a surface whose render throws draws an alert in its place, whose Close gives the page back, and the next open draws it again", async () => {
  const user = userEvent.setup();
  let throws = true;
  function Fragile({ open }: AskedSurfaceProps): ReactNode {
    if (open !== false && throws) throw new Error("The surface failed to render");
    return open === false ? null : <p>Fragile drawn</p>;
  }
  const source = chunk(async () => Fragile);
  await source.load();
  const logged = quietConsoleError();
  const reload = spyOn(window.location, "reload").mockImplementation(() => undefined);
  const view = render(<Shell source={source} />);
  try {
    const opener = screen.getByRole("button", { name: "Open" });
    await user.click(opener);
    const alert = screen.getByRole("alertdialog", { name: "Could not open Example." });
    expect(alert).toHaveTextContent(/^Could not open Example\./u);
    expect(logged).toHaveBeenCalled();
    await user.click(within(alert).getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledTimes(1);
    await user.click(within(alert).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(opener).toHaveFocus();
    throws = false;
    await user.click(opener);
    expect(screen.getByText("Fragile drawn")).toBeDefined();
  } finally {
    reload.mockRestore();
    logged.mockRestore();
    view.unmount();
  }
});

// A chunk the surface draws from that fails to download online - a decision card in a body of
// the peek, say - draws the same alert: the boundary names it for a chunk that could not load as
// well as for a render that threw (`notLoaded`), so the surface does not go with no word.
test("a chunk the surface draws from that fails to download draws the alert in its place", async () => {
  const user = userEvent.setup();
  const Card = failingChunkComponent();
  function WithCard({ open }: AskedSurfaceProps): ReactNode {
    return open === false ? null : <Card />;
  }
  const source = chunk(async () => WithCard);
  await source.load();
  const logged = quietConsoleError();
  const view = render(<Shell source={source} />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    const alert = await screen.findByRole("alertdialog", { name: "Could not open Example." });
    expect(alert).toHaveTextContent(/^Could not open Example\./u);
  } finally {
    logged.mockRestore();
    view.unmount();
  }
});

// An open the reader called off leaves its download to finish as a prefetch: nobody waits on it
// any more, so its failure - once the network is back after an outage the reader called the open
// off in, say - is no failure a reader is in front of, and the chunk-failure recovery reads it as
// a prefetch's and reloads nothing (`onlyPrefetchesOut`, `DeploymentResilience.tsx`). Counted as
// a render until it ended, it would spend one of the tab's reloads under a reader who had gone
// on.
test("an open called off while its chunk is out leaves the download to finish as a prefetch", async () => {
  const user = userEvent.setup();
  // From a page with no download out: the counts are the module's, for the page, and a download
  // an earlier row of this file started and did not wait for, or a row of an earlier run of it
  // (`--rerun-each`, which keeps the file's modules), would decide this one. A row of another file
  // cannot: `--parallel` gives every file modules of its own.
  await waitFor(() => expect(chunkLoadsOut()).toBe(0), { timeout: 30_000 });
  const { promise, reject } = Promise.withResolvers<typeof Surface>();
  const view = render(<Shell source={chunk(() => promise)} />);
  try {
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(onlyPrefetchesOut(), "the open's download was not counted as a render").toBe(false);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("textbox", { name: LOADING })).not.toBeInTheDocument();
    expect(chunkLoadsOut(), "the called-off download was not left to finish").toBe(1);
    expect(onlyPrefetchesOut(), "the called-off download still counts as a render").toBe(true);
    await act(async () => reject(new TypeError("Failed to fetch dynamically imported module")));
    expect(chunkLoadsOut()).toBe(0);
  } finally {
    reject(new TypeError("Failed to fetch dynamically imported module"));
    view.unmount();
  }
});
