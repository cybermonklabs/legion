import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { type ReactNode, Suspense, use } from "react";

import { chunkComponent } from "./chunk-component";

afterEach(cleanup);

/** A part that suspends until `ready` settles, so the lane its arrival commits in is the lane the
 *  boundary around it answers to: a default-lane arrival replaces what is on the screen with the
 *  boundary's fallback, a transition's keeps what the reader has until the part can be drawn. */
function suspendingPart(ready: Promise<void>): () => ReactNode {
  return function Part(): ReactNode {
    use(ready);
    return <p>the part</p>;
  };
}

/** The chunk's own answer, and the part it answers with. */
function arrival(): {
  land: () => void;
  load: () => Promise<() => ReactNode>;
  ready: () => void;
} {
  const chunk = Promise.withResolvers<() => ReactNode>();
  const ready = Promise.withResolvers<void>();
  const part = suspendingPart(ready.promise);
  return {
    land: () => chunk.resolve(part),
    load: () => chunk.promise,
    ready: () => ready.resolve(),
  };
}

// The arrival lands in a promise callback, which React gives the default lane: the whole of the
// part's mount then renders in one task that never yields, and a host that opened a transition
// around this component bought nothing, because that transition was over before the chunk
// answered (dispatch://INFRA-341, R11). `inATransition` is the arrival saying it for itself, and
// the lane is what the boundary above reads: a transition does not take the reader's screen away
// for a fallback.
test("a part whose arrival is a transition keeps what is drawn while its render suspends", async () => {
  const { land, load, ready } = arrival();
  const Part = chunkComponent(load, <p>the space</p>, true);
  render(
    <Suspense fallback={<p>the boundary</p>}>
      <Part />
    </Suspense>
  );

  expect(screen.getByText("the space")).toBeTruthy();
  land();
  await Bun.sleep(20);
  expect(screen.queryByText("the boundary")).not.toBeInTheDocument();
  expect(screen.getByText("the space")).toBeTruthy();

  ready();
  await waitFor(() => expect(screen.getByText("the part")).toBeTruthy());
});

// Every other part arrives as it always did: nothing about its first render is worth splitting,
// and the space it keeps is replaced in the commit the chunk arrives in, boundary and all.
test("a part that names no transition arrives at the default lane", async () => {
  const { land, load, ready } = arrival();
  const Part = chunkComponent(load, <p>the space</p>);
  render(
    <Suspense fallback={<p>the boundary</p>}>
      <Part />
    </Suspense>
  );

  expect(screen.getByText("the space")).toBeTruthy();
  land();
  await waitFor(() => expect(screen.getByText("the boundary")).toBeTruthy());

  ready();
  await waitFor(() => expect(screen.getByText("the part")).toBeTruthy());
});
