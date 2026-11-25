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
// that, or nothing, and never the fallback a host names for a render that threw: a list of writes
// whose chunk cannot load draws nothing, as it does with no write to show, while one whose render
// threw draws an alert (`features/writes/write-hosts.tsx`).
test("a chunk under it that fails to download draws what the host names for that, or nothing, and never the fallback", async () => {
  const NotLoaded = failingChunkComponent();
  const logged = quietConsoleError();
  const view = render(
    <div>
      <section aria-label="Quiet">
        <ChunkBoundary fallback={<p>Could not show it</p>}>
          <NotLoaded />
        </ChunkBoundary>
      </section>
      <ChunkBoundary fallback={<p>Could not show it</p>} notLoaded={<p>Could not load it</p>}>
        <NotLoaded />
      </ChunkBoundary>
    </div>
  );
  try {
    await waitFor(() => expect(logged).toHaveBeenCalledTimes(2));
    expect(screen.getByText("Could not load it")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Quiet" })).toBeEmptyDOMElement();
    expect(screen.queryByText("Could not show it")).not.toBeInTheDocument();
  } finally {
    logged.mockRestore();
    view.unmount();
  }
});
