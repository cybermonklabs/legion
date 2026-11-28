import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderOptions, type RenderResult, render, renderHook } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";

/** Who every client of this module, and `signedInQueryClient`, is signed in as: alice, a person,
 *  whose sign-in holds the `person` scope that the reads of the people and sessions a body names
 *  are made under (`useHoldsPersonScope`). */
export const SIGNED_IN_PERSON = { event_head: "0", kind: "user", login: "alice" } as const;

/** A client with no retries, signed in as alice (`SIGNED_IN_PERSON`), as a page's client is once
 *  the sign-in check has answered. */
export function signedInQueryClient(): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], SIGNED_IN_PERSON);
  return queryClient;
}

/**
 * `render` with a query client around what it draws. Every Markdown surface reads what its text
 * cites - the issue keys it writes bare, the question map of its document, the item each id
 * prefix names (`useTextReferences`), the people and sessions it names - so a body needs a client
 * wherever it is drawn, as it has one everywhere in the app, signed in as the app's is. Each
 * render gets its own, as a fresh page does, and the wrapper is the render's own, so a `rerender`
 * keeps it. A wrapper the caller gives is kept, inside this one; a client it provides there nests
 * inside this one and wins.
 */
export function renderWithQueries(
  ui: Parameters<typeof render>[0],
  options?: RenderOptions
): RenderResult {
  const queryClient = signedInQueryClient();
  const Given = options?.wrapper;
  const Wrapper = ({ children }: { children: ReactNode }): ReactElement => (
    <QueryClientProvider client={queryClient}>
      {Given === undefined ? children : <Given>{children}</Given>}
    </QueryClientProvider>
  );
  return render(ui, { ...options, wrapper: Wrapper });
}

/** `useWrite` rendered over one app's queue, with alice signed in. */
export function renderOnQueue<T>(useWrite: () => T) {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], SIGNED_IN_PERSON);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return { client, hook: renderHook(useWrite, { wrapper }) };
}
