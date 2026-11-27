import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { pressHeld } from "../__tests__/held-press";
import { QueryError } from "./QueryError";

/** A read drawn as the Artifacts tab draws its references: its answer, or, once a read of it
 *  failed, the line that says so, with Retry held while a read is out. */
function Read({ read }: { read: () => Promise<string> }): ReactNode {
  const query = useQuery({ queryFn: read, queryKey: ["read"] });
  return query.isError ? (
    <QueryError
      message="References unavailable"
      onRetry={() => void query.refetch()}
      retrying={query.isFetching}
    />
  ) : (
    <p>{query.data}</p>
  );
}

// A read that answered and then failed draws the line over the answer it still holds, so the line
// stands while the read Retry sends is out. Retry is held then, never disabled: the browser takes
// the focus off a control that turns disabled, which would leave the keyboard reader who pressed it
// on the page's body. Held, it keeps the focus, and a second press sends no second read.
test("Retry keeps the focus while the read it sent is out, and a press then sends nothing", async () => {
  const held = Promise.withResolvers<string>();
  let reads = 0;
  const read = async (): Promise<string> => {
    reads += 1;
    if (reads === 1) return "One reference";
    if (reads === 2) throw new Error("The read failed.");
    return held.promise;
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <Read read={read} />
    </QueryClientProvider>
  );
  try {
    await screen.findByText("One reference");
    await act(() => client.refetchQueries());
    const retry = await screen.findByRole("button", { name: "Retry" });
    await pressHeld(userEvent.setup(), retry, { name: "Retry", sent: () => reads - 2 });

    await act(async () => held.resolve("Two references"));
    expect(await screen.findByText("Two references")).toBeTruthy();
  } finally {
    view.unmount();
    client.clear();
  }
});
