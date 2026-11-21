// The shell's own parts are drawn after the editor's first batch on an address that opens a
// document, and the connection pill is not one of them (dispatch://INFRA-341 R11,
// `features/doc/AfterTheFirstBatch.tsx`). A hold over it would run longest on exactly the load
// whose socket is gone, and the corner it stands in keeps no box, so a reader who is not told
// has nothing there to press for.
import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";

import { installFakeLiveSockets } from "../../__tests__/live-socket-harness";
import { liveSocketFor } from "../../api/live-socket";
import { theFirstBatchIsAwaited } from "../doc/first-batch";
import { ConnectionPill } from "./ConnectionPill";

afterEach(() => {
  cleanup();
  theFirstBatchIsAwaited(false);
});

test("the connection pill says the socket is lost while the page is still holding its chrome", () => {
  const sockets = installFakeLiveSockets();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The page's event feed is what opens the socket the pill reads, as it does in the shell.
  const stopEvents = liveSocketFor(queryClient).subscribeEvents({
    onEvent: () => {},
    onStart: () => {},
    since: () => null,
  });
  // Armed before anything renders, as an address that opens a document arms it
  // (`openDocumentEarly`): a part that reads the hold on its first render is held by it.
  theFirstBatchIsAwaited(true);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ConnectionPill />
    </QueryClientProvider>
  );

  try {
    act(() => sockets.latest().open());
    // Nothing to say while the socket is up, which is every second of a load that goes well.
    expect(screen.queryByTestId("connection-pill")).toBeNull();

    act(() => sockets.latest().drop(1006));
    expect(screen.getByTestId("connection-pill").textContent).toBe("Reconnecting…");
  } finally {
    stopEvents();
    view.unmount();
    liveSocketFor(queryClient).close();
    sockets.restore();
  }
});
