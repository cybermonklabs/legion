import { beforeAll, expect, jest, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";
import { countEngineParses, displayTree } from "../../__tests__/display-trees";
import { jump, realClock } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { PriorityDescription } from "./PriorityDescription";
import { SAVE_AFTER_MS } from "./useInPlaceDraft";

/**
 * A writer's description over a server the test answers by hand, from outside the page, since a
 * click would take the focus out of the editor. Every text the editor sends is recorded in
 * `saves`, in order, and stays out until the test refuses it (`refuse`) or a read brings a text
 * back as the description (`lands`).
 */
function heldDescription(initial: string) {
  const saves: string[] = [];
  const out: { readonly refuse: (refusal: Error) => void; readonly text: string }[] = [];
  let read: (text: string) => void = () => {
    throw new Error("the description has not rendered");
  };
  const Description = (): ReactNode => {
    const [description, setDescription] = useState(initial);
    read = setDescription;
    return (
      <PriorityDescription
        canWrite
        description={description}
        // The texts this row reads are the test's own, which the server read no tree of.
        descriptionTree={undefined}
        label="Description of P2"
        onSave={(next) => {
          saves.push(next);
          const write = Promise.withResolvers<void>();
          out.push({ refuse: write.reject, text: next });
          return write.promise;
        }}
      />
    );
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={client}>
        <Description />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    lands: (text: string) => act(() => read(text)),
    page: within(view.container),
    /** Refuses the newest write of `text` still out. */
    refuse: (text: string) =>
      act(async () => {
        const at = out.map((write) => write.text).lastIndexOf(text);
        const [write] = at === -1 ? [] : out.splice(at, 1);
        if (write === undefined) throw new Error(`no write of ${text} is out`);
        write.refuse(new Error("refused"));
      }),
    saves,
    unmount: () => {
      view.unmount();
      client.clear();
    },
  };
}

// The rendered block reads what Dispatch is about to hold: the reader's text while its write is
// out, and Dispatch's own once the write is refused. The refused text is still the reader's in
// the editor, and leaving the editor sends it again.
test("a refused description reads as Dispatch's text again, and the next leave sends it again", async () => {
  const user = userEvent.setup();
  const { page, refuse, saves, unmount } = heldDescription("Speedruns run again.");
  const more = "Speedruns run again. More.";
  try {
    await user.click(await page.findByText("Speedruns run again."));
    await user.type(await page.findByRole("textbox", { name: "Description of P2" }), " More.");
    await user.tab();
    expect(saves).toEqual([more]);
    expect(await page.findByText(more)).toBeDefined();
    await refuse(more);
    await user.click(await page.findByText("Speedruns run again."));
    const reopened = await page.findByRole<HTMLTextAreaElement>("textbox", {
      name: "Description of P2",
    });
    expect(reopened.value).toBe(more);
    await user.tab();
    expect(saves).toEqual([more, more]);
  } finally {
    unmount();
  }
});

// The read after the first of two writes lands while the reader still holds the editor: the
// second text is what Dispatch is about to hold, so leaving sends nothing more.
test("the answer to an earlier description write landing under the reader sends the later text once", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { lands, page, saves, unmount } = heldDescription("");
  try {
    await user.click(page.getByText(/No description is written\./));
    const editor = await page.findByRole("textbox", { name: "Description of P2" });
    await user.type(editor, "A");
    await jump(SAVE_AFTER_MS + 1);
    await user.type(editor, "B");
    await jump(SAVE_AFTER_MS + 1);
    expect(saves).toEqual(["A", "AB"]);
    await lands("A");
    await user.tab();
    expect(saves).toEqual(["A", "AB"]);
  } finally {
    realClock();
    unmount();
  }
});

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// A description is a stored text its writer typed in a box with the reference picker, and the
// priorities read sends the reading the server made of it (`description_tree`): the rendered
// block draws that tree and parses nothing on the page's thread. The tree reads differently from
// the markdown beside it, so a block drawn from a parse of the markdown would end `daemon.`.
test("a description is drawn from the tree the priorities read sends", async () => {
  const tree = displayTree("Ship the **daemon** first.");
  const parses = countEngineParses();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={client}>
        <PriorityDescription
          canWrite={false}
          description="Ship the **daemon**."
          descriptionTree={tree}
          label="Description of P1"
          onSave={async () => undefined}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() => expect(view.container.textContent).toBe("Ship the daemon first."));
    expect(view.container.querySelector("strong")?.textContent).toBe("daemon");
    expect(parses.count()).toBe(0);
  } finally {
    parses.restore();
    view.unmount();
    client.clear();
  }
});
