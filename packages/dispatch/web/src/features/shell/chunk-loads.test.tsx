import { expect, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import { type ReactNode, Suspense } from "react";

import { settle } from "../../__tests__/fake-clock";
import { withOnLine } from "../../__tests__/on-line";
import { importWhenOnline, keptUntilRejected, lazyChunk } from "./chunk-loads";
import { ErrorBoundary } from "./ErrorBoundary";

function Greeting({ name }: { name: string }): ReactNode {
  return <p>Hello {name}</p>;
}

test("a chunk that fails offline loads once the browser is back online", async () => {
  let attempts = 0;
  const load = () => {
    attempts += 1;
    return attempts === 1
      ? Promise.reject(new TypeError("Failed to fetch"))
      : Promise.resolve("ok");
  };

  await withOnLine(false, async () => {
    const loading = importWhenOnline(load);
    await Promise.resolve();
    expect(attempts).toBe(1);
    window.dispatchEvent(new Event("online"));
    expect(await loading).toBe("ok");
    expect(attempts).toBe(2);
  });
});

test("a chunk that fails while online is a real failure, not a network blip", async () => {
  await withOnLine(true, async () => {
    await expect(
      importWhenOnline(() => Promise.reject(new TypeError("Failed to fetch")))
    ).rejects.toThrow("Failed to fetch");
  });
});

test("a kept load that rejects is asked again by the next call, even from the failure's own handler", async () => {
  let loads = 0;
  const kept = keptUntilRejected(() => {
    loads += 1;
    return loads === 1 ? Promise.reject(new TypeError("Failed to fetch")) : Promise.resolve("ok");
  });

  expect(await kept().catch(() => kept())).toBe("ok");
  expect(loads).toBe(2);
});

test("a kept load that resolves is asked once", async () => {
  let loads = 0;
  const kept = keptUntilRejected(() => {
    loads += 1;
    return Promise.resolve(loads);
  });

  expect(await kept()).toBe(1);
  expect(await kept()).toBe(1);
  expect(loads).toBe(1);
});

test("two calls while a kept load is out share that one load", async () => {
  let loads = 0;
  const download = Promise.withResolvers<string>();
  const kept = keptUntilRejected(() => {
    loads += 1;
    return download.promise;
  });

  const first = kept();
  const second = kept();
  expect(second).toBe(first);
  expect(loads).toBe(1);
  download.resolve("ok");
  expect(await second).toBe("ok");
});

test("a lazy component whose download fails offline draws once the browser is back online", async () => {
  let loads = 0;
  const Lazy = lazyChunk(() => {
    loads += 1;
    return loads === 1
      ? Promise.reject(new TypeError("Failed to fetch dynamically imported module"))
      : Promise.resolve({ default: Greeting });
  });

  await withOnLine(false, async () => {
    const view = render(
      <ErrorBoundary region="this panel">
        <Suspense fallback={<p>Loading</p>}>
          <Lazy name="Ada" />
        </Suspense>
      </ErrorBoundary>
    );
    try {
      await act(settle);
      expect(loads).toBe(1);
      expect(screen.queryByRole("alert")).toBeNull();
      expect(screen.getByText("Loading")).toBeDefined();

      await act(async () => {
        window.dispatchEvent(new Event("online"));
        await settle();
      });
      expect(await screen.findByText("Hello Ada")).toBeDefined();
      expect(loads).toBe(2);
    } finally {
      // Sent here too, so a row that fails before the event above leaves no download waiting on
      // it: that download is under `renderChunk`, and while it is out `chunkLoadsOut` stays above
      // zero for the later rows of this file. The second load resolves, and an `online` nothing
      // waits on reaches no listener.
      await act(async () => {
        window.dispatchEvent(new Event("online"));
        await settle();
      });
      view.unmount();
    }
  });
});
