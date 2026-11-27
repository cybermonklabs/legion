import { expect, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import { type ReactNode, useEffect } from "react";

import { settle } from "../../__tests__/fake-clock";
import { chunk, chunkComponent } from "./chunk-component";
import { ErrorBoundary } from "./ErrorBoundary";

function Greeting({ name }: { name: string }): ReactNode {
  return <p>Hello {name}</p>;
}

test("a chunk that is already in draws in its parent's first commit", async () => {
  const source = chunk(async () => Greeting);
  await source.load();
  const Loaded = chunkComponent(source, <p>Loading</p>);
  // What the parent's effect finds in the page is what its own commit drew.
  const seen: string[] = [];
  function Parent(): ReactNode {
    useEffect(() => {
      seen.push(document.body.textContent ?? "");
    }, []);
    return <Loaded name="Ada" />;
  }
  const view = render(<Parent />);
  try {
    expect(seen).toEqual(["Hello Ada"]);
  } finally {
    view.unmount();
  }
});

test("a chunk that is not in draws the fallback, then the component", async () => {
  const { promise, resolve } = Promise.withResolvers<typeof Greeting>();
  let imports = 0;
  const source = chunk(() => {
    imports += 1;
    return promise;
  });
  const Loading = chunkComponent(source, <p>Loading</p>);
  const view = render(
    <>
      <Loading name="Ada" />
      <Loading name="Grace" />
    </>
  );
  try {
    expect(screen.getAllByText("Loading")).toHaveLength(2);
    await act(async () => resolve(Greeting));
    expect(screen.getByText("Hello Ada")).toBeDefined();
    expect(screen.getByText("Hello Grace")).toBeDefined();
    // Two mounts, one download.
    expect(imports).toBe(1);
  } finally {
    // The download goes through `renderChunk`, so one still out would stay counted for the later
    // rows of this file (`chunkLoadsOut`). Resolving a settled promise does nothing.
    resolve(Greeting);
    view.unmount();
  }
});

test("a download that fails reaches the error boundary, and the next load asks again", async () => {
  let imports = 0;
  const source = chunk<{ name: string }>(() => {
    imports += 1;
    return Promise.reject(new Error("Failed to fetch dynamically imported module"));
  });
  const Failing = chunkComponent(source, <p>Loading</p>);
  const consoleError = console.error;
  console.error = () => {};
  const view = render(
    <ErrorBoundary region="this panel">
      <Failing name="Ada" />
    </ErrorBoundary>
  );
  try {
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Failed to fetch dynamically imported module"
    );
    await expect(source.load()).rejects.toThrow("Failed to fetch");
    expect(imports).toBe(2);
  } finally {
    console.error = consoleError;
    view.unmount();
  }
});

test("a download that fails offline draws the fallback, reaches no error boundary, and draws the component once the browser is back online", async () => {
  const first = Promise.withResolvers<typeof Greeting>();
  const second = Promise.withResolvers<typeof Greeting>();
  let imports = 0;
  const source = chunk<{ name: string }>(() => {
    imports += 1;
    return imports === 1 ? first.promise : second.promise;
  });
  const Loading = chunkComponent(source, <p>Loading</p>);
  const onLine = Object.getOwnPropertyDescriptor(Navigator.prototype, "onLine");
  Object.defineProperty(navigator, "onLine", { configurable: true, value: false });
  const view = render(
    <ErrorBoundary region="this panel">
      <Loading name="Ada" />
    </ErrorBoundary>
  );
  try {
    await act(async () => {
      first.reject(new TypeError("Failed to fetch dynamically imported module"));
      await settle();
    });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("Loading")).toBeDefined();
    expect(imports).toBe(1);

    await act(async () => {
      window.dispatchEvent(new Event("online"));
      await settle();
    });
    expect(imports).toBe(2);
    await act(async () => {
      second.resolve(Greeting);
      await settle();
    });
    expect(await screen.findByText("Hello Ada")).toBeDefined();
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    // Every download settled and the event sent, whichever step failed: a download still out
    // through `renderChunk` would stay counted for the later rows of this file (`chunkLoadsOut`),
    // and one still waiting on `online` would never end. Settling a settled
    // promise does nothing, and an `online` nothing waits on reaches no listener.
    first.reject(new TypeError("Failed to fetch dynamically imported module"));
    window.dispatchEvent(new Event("online"));
    second.resolve(Greeting);
    view.unmount();
    Reflect.deleteProperty(navigator, "onLine");
    if (onLine !== undefined) {
      Object.defineProperty(Navigator.prototype, "onLine", onLine);
    }
  }
});
