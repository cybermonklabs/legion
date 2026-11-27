import { expect, test } from "bun:test";
import { render, screen, waitFor } from "@testing-library/react";

import { quietConsoleError } from "../../__tests__/act-warnings";
import { failingChunkComponent } from "../../__tests__/failing-chunk";
import { ChunkBoundary } from "./ChunkBoundary";

function Throwing(): never {
  throw new Error("The part failed to render");
}

// A part whose render throws draws nothing in its place, or the fallback its host names for it,
// and the rest of the page stays drawn: the throw never reaches a boundary above.
test("a render that throws under it draws nothing, or the fallback a host names, and its siblings stay", () => {
  const logged = quietConsoleError();
  const view = render(
    <div>
      <p>Signed in as alice</p>
      <section aria-label="Quiet">
        <ChunkBoundary>
          <Throwing />
        </ChunkBoundary>
      </section>
      <ChunkBoundary fallback={<p>The shell before</p>}>
        <Throwing />
      </ChunkBoundary>
    </div>
  );
  try {
    expect(screen.getByRole("region", { name: "Quiet" })).toBeEmptyDOMElement();
    expect(screen.getByText("The shell before")).toBeTruthy();
    expect(screen.getByText("Signed in as alice")).toBeTruthy();
    expect(logged).toHaveBeenCalled();
  } finally {
    logged.mockRestore();
    view.unmount();
  }
});

test("a part that renders draws as it is", () => {
  const view = render(
    <ChunkBoundary fallback={<p>The shell before</p>}>
      <p>content</p>
    </ChunkBoundary>
  );
  try {
    expect(screen.getByText("content")).toBeTruthy();
    expect(screen.queryByText("The shell before")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A chunk under it that fails to download online (`ChunkNotLoaded`) draws what its host names for
// that, or else the fallback its host names for a render that threw, or nothing when the host
// names neither: a host that names only `fallback` is never silent when its chunk fails. The
// pending-writes host names one of its own for that (`NotLoaded`,
// `features/writes/write-hosts.tsx`), which draws the indicator's loading fallback and has the
// line the host draws beside the boundary count the indicator's writes.
test("a chunk under it that fails to download draws what the host names for that, or else its fallback, or nothing", async () => {
  const NotLoaded = failingChunkComponent();
  const logged = quietConsoleError();
  const view = render(
    <div>
      <section aria-label="Quiet">
        <ChunkBoundary>
          <NotLoaded />
        </ChunkBoundary>
      </section>
      <section aria-label="Fallback only">
        <ChunkBoundary fallback={<p>Could not show it</p>}>
          <NotLoaded />
        </ChunkBoundary>
      </section>
      <section aria-label="Both">
        <ChunkBoundary fallback={<p>Could not show it</p>} notLoaded={<p>Could not load it</p>}>
          <NotLoaded />
        </ChunkBoundary>
      </section>
    </div>
  );
  try {
    await waitFor(() => expect(logged).toHaveBeenCalledTimes(3));
    expect(screen.getByRole("region", { name: "Quiet" })).toBeEmptyDOMElement();
    expect(screen.getByRole("region", { name: "Fallback only" })).toHaveTextContent(
      /^Could not show it$/u
    );
    expect(screen.getByRole("region", { name: "Both" })).toHaveTextContent(/^Could not load it$/u);
  } finally {
    logged.mockRestore();
    view.unmount();
  }
});
