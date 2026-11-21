import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the Enter that ends an input method's composition below, which user-event cannot mark as one
import { act, fireEvent, render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { jump, onFakeClock, settle } from "../../__tests__/fake-clock";
import { createApiClient, priorityPatchRequest } from "../../api/client";
import { InPlaceField, SAVE_AFTER_MS } from "./InPlaceField";

/**
 * The field as a page holds it: `value` is the text the server answered with, and it moves only
 * when something writes it — the reader's own write coming back, or another writer's. The two
 * buttons stand for those two moments, so a test says when each happens rather than waiting on
 * a mutation it cannot see.
 */
function Field(): ReactNode {
  const [value, setValue] = useState("Old name");
  const [sent, setSent] = useState<string>();
  return (
    <>
      <InPlaceField label="Title" onSave={setSent} value={value} />
      <button onClick={() => setValue(sent ?? value)} type="button">
        Land the write
      </button>
      <button onClick={() => setValue("Written elsewhere")} type="button">
        Write it elsewhere
      </button>
    </>
  );
}

// Queries are scoped to this render's container: bun runs every test file in one document, and
// a sibling file's un-unmounted render would otherwise leak its fields into a `screen` query.
test("the text a reader left stands through the write, rather than the old text returning", async () => {
  const user = userEvent.setup();
  const view = render(<Field />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" });
  await user.clear(field);
  await user.type(field, "New name");
  // Leaving the field sends the write. The answer is not back, so the value under the field is
  // still the old text; the reader must not see their own words undone while it travels.
  await user.tab();
  expect((field as HTMLInputElement).value).toBe("New name");
  await user.click(page.getByRole("button", { name: "Land the write" }));
  expect((field as HTMLInputElement).value).toBe("New name");
  view.unmount();
});

test("a text written elsewhere replaces what a field the reader is not holding shows", async () => {
  const user = userEvent.setup();
  const view = render(<Field />);
  const page = within(view.container);
  await user.click(page.getByRole("button", { name: "Write it elsewhere" }));
  expect((page.getByRole("textbox", { name: "Title" }) as HTMLInputElement).value).toBe(
    "Written elsewhere"
  );
  view.unmount();
});

/** The field as a page holds it while another person's write lands: every write the field sends
 *  is kept in `writes`, and `land.write` stands for the server's answer moving `value` under the
 *  field, as a refresh of the row does, without taking the focus from it. */
function HeldField({
  land,
  required,
  writes,
}: {
  land: { write?: (text: string) => void };
  required?: boolean;
  writes: string[];
}): ReactNode {
  const [value, setValue] = useState("Old name");
  land.write = setValue;
  return (
    <InPlaceField
      label="Title"
      onSave={(next) => writes.push(next)}
      required={required}
      value={value}
    />
  );
}

test("leaving a held field with nothing typed writes nothing, and shows what was written meanwhile", async () => {
  const user = userEvent.setup();
  const writes: string[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(<HeldField land={land} writes={writes} />);
  const field = within(view.container).getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  await user.click(field);
  // Another person's rename lands while the reader holds the field.
  act(() => land.write?.("Written elsewhere"));
  expect(document.activeElement).toBe(field);
  await user.tab();
  // The reader typed nothing, so leaving writes nothing. A write of "Old name" here takes the
  // other person's rename back, which nobody asked for, and the field goes on showing it.
  expect(writes).toEqual([]);
  expect(field.value).toBe("Written elsewhere");
  view.unmount();
});

/** The field as `HeldField` draws it, with what a test reads of it and the notice beside it. */
function heldField(required = false) {
  const writes: string[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(<HeldField land={land} required={required} writes={writes} />);
  const page = within(view.container);
  return {
    field: page.getByRole("textbox", { name: "Title" }) as HTMLInputElement,
    land: (text: string) => act(() => land.write?.(text)),
    notice: () => page.queryByRole("alert", { name: "Title, changed by another person" }),
    view,
    writes,
  };
}

// The rows below type on the fake clock, with no wait between keys (`delay: null`), so the write
// that waits for the typing to rest goes only when a row moves the clock.
test("another person's text landing under the reader's typing is shown, theirs kept beside it, and written only when they use it", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, land, notice, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    land("Written elsewhere");
    // The write that waited for the typing to rest is stopped: it would write over theirs.
    await jump(SAVE_AFTER_MS);
  });
  expect(writes).toEqual([]);
  expect(field.value).toBe("Written elsewhere");
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the field");
  expect(kept.textContent).toContain("Another person changed this while you wrote. Your text:");
  expect(within(kept).getByText("Old name, mine")).toBeDefined();
  // Leaving the field writes nothing: it holds the server's text.
  await user.tab();
  expect(writes).toEqual([]);
  await user.click(within(kept).getByRole("button", { name: "Use my text" }));
  expect(writes).toEqual(["Old name, mine"]);
  expect(field.value).toBe("Old name, mine");
  expect(notice()).toBeNull();
  // Its round trip is the reader's own text coming back, and no other person's.
  land("Old name, mine");
  expect(notice()).toBeNull();
  view.unmount();
  expect(writes).toEqual(["Old name, mine"]);
});

test("keeping the other person's text lets the reader's go, and writes nothing", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, land, notice, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    land("Written elsewhere");
  });
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the field");
  await user.click(within(kept).getByRole("button", { name: "Keep their text" }));
  expect(notice()).toBeNull();
  expect(field.value).toBe("Written elsewhere");
  await user.tab();
  view.unmount();
  expect(writes).toEqual([]);
});

test("the answer to a write made as the reader types is their own text, trimmed as it is stored, and leaves the typing alone", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, land, notice, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, " now ");
    await jump(SAVE_AFTER_MS);
    expect(writes).toEqual(["Old name now"]);
    await user.type(field, "and then");
    land("Old name now");
  });
  expect(notice()).toBeNull();
  expect(field.value).toBe("Old name now and then");
  await user.tab();
  expect(writes).toEqual(["Old name now", "Old name now and then"]);
  view.unmount();
  expect(writes).toEqual(["Old name now", "Old name now and then"]);
});

test("a field taken off the page while its typing waits writes it", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, " now");
    expect(writes).toEqual([]);
    view.unmount();
    expect(writes).toEqual(["Old name now"]);
    await jump(SAVE_AFTER_MS);
  });
  expect(writes).toEqual(["Old name now"]);
});

test("the page going while the typing waits writes it, once", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, " now");
    expect(writes).toEqual([]);
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(writes).toEqual(["Old name now"]);
    // That was the write waiting for the typing to rest: none goes once the typing has rested.
    await jump(SAVE_AFTER_MS);
  });
  expect(writes).toEqual(["Old name now"]);
  view.unmount();
  expect(writes).toEqual(["Old name now"]);
});

// A write made as the page goes is the tab closing as often as a reload, and no reload follows a
// closed tab to make it again, so it is let finish as the page goes (`keepalive`), its key and all
// (R14). A write made while the page stays - the reader leaving the field by Enter - is not: a
// reload makes it again under its key, as it makes every write (`api/client.ts`'s `send`).
test("the write the page going makes is let finish as the page goes, and one made while the page stays is not", async () => {
  const user = userEvent.setup({ delay: null });
  const sent: RequestInit[] = [];
  const client = createApiClient(async (_path, init) => {
    sent.push(init ?? {});
    return Response.json({});
  });
  let pressed = 0;
  const view = render(
    <InPlaceField
      label="Title"
      onSave={(title) => {
        pressed += 1;
        void client.send(priorityPatchRequest("rocq-lean", { title }), { key: `press-${pressed}` });
      }}
      value="Old name"
    />
  );
  const field = within(view.container).getByRole("textbox", { name: "Title" });
  await onFakeClock(async () => {
    await user.type(field, ", left{Enter}");
    await user.type(field, ", gone");
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    await settle();
  });
  view.unmount();
  expect(
    sent.map((init) => ({
      body: init.body,
      key: new Headers(init.headers).get("Idempotency-Key"),
      keepalive: init.keepalive,
      method: init.method,
    }))
  ).toEqual([
    {
      body: JSON.stringify({ title: "Old name, left" }),
      key: "press-1",
      keepalive: undefined,
      method: "PATCH",
    },
    {
      body: JSON.stringify({ title: "Old name, left, gone" }),
      key: "press-2",
      keepalive: true,
      method: "PATCH",
    },
  ]);
});

// The Enter that ends an input method's composition - of Japanese, Chinese or Korean - takes the
// word being composed, and is no Enter of the reader's: it neither writes the text nor leaves the
// field. Chromium and Firefox mark it `isComposing`; Safari sends it after the composition's end,
// unmarked, with keyCode 229 (`inputMethodKey`). A plain Enter does both.
test.each([
  ["isComposing", { isComposing: true, key: "Enter" }],
  ["keyCode 229", { isComposing: false, key: "Enter", keyCode: 229 }],
] as const)("the Enter that ends a composition, marked by %s, neither writes nor leaves the field, and a plain one does both", async (_mark, composing) => {
  const user = userEvent.setup({ delay: null });
  const { field, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(field, composing);
    expect(writes).toEqual([]);
    expect(document.activeElement).toBe(field);
    await user.keyboard("{Enter}");
    expect(writes).toEqual(["Old name, mine"]);
    expect(document.activeElement).not.toBe(field);
  });
  view.unmount();
  expect(writes).toEqual(["Old name, mine"]);
});

test("the answer to an earlier write the reader typed back to leaves the later write out, and writes the reader's text again", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, land, notice, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS);
    expect(writes).toEqual(["Old name A", "Old name AB"]);
    // The reader takes the B back, and the first write's answer arrives while that typing rests.
    await user.type(field, "{Backspace}");
    land("Old name A");
    await jump(SAVE_AFTER_MS);
  });
  // "Old name AB" is still out, and lands after "Old name A": the reader's text goes again.
  expect(writes).toEqual(["Old name A", "Old name AB", "Old name A"]);
  // That later answer is the reader's own write coming back, and is not taken in over theirs.
  land("Old name AB");
  expect(field.value).toBe("Old name A");
  expect(notice()).toBeNull();
  view.unmount();
  expect(writes).toEqual(["Old name A", "Old name AB", "Old name A"]);
});

test("a second change while the notice shows keeps both of the reader's texts, and the one they use is written", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, land, notice, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    land("Written elsewhere");
    // The reader types over the other person's text without choosing, and a second change lands.
    await user.type(field, ", mine again");
    land("Written elsewhere twice");
    await jump(SAVE_AFTER_MS);
  });
  expect(writes).toEqual([]);
  expect(field.value).toBe("Written elsewhere twice");
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the field");
  expect(kept.textContent).toContain(
    "Another person changed this while you wrote. Your texts, newest first:"
  );
  expect(
    within(kept)
      .getAllByText(/mine/)
      .map((text) => text.textContent)
  ).toEqual(["Written elsewhere, mine again", "Old name, mine"]);
  expect(within(kept).getAllByRole("button", { name: "Keep their text" })).toHaveLength(1);
  const uses = within(kept).getAllByRole("button", { name: "Use this text" });
  expect(uses).toHaveLength(2);
  // The older one, put back and written at once; the other goes with the notice.
  await user.click(uses[1]);
  expect(writes).toEqual(["Old name, mine"]);
  expect(field.value).toBe("Old name, mine");
  expect(notice()).toBeNull();
  view.unmount();
  expect(writes).toEqual(["Old name, mine"]);
});

/** The field as a page draws it that holds the reader's kept texts rather than the field, as
 *  `usePriorityWrites` does in the write store, and takes the field off the page and draws it
 *  again, as a closed disclosure or a table row scrolled away does. */
function PageHeldField({
  land,
  writes,
}: {
  land: { write?: (text: string) => void };
  writes: string[];
}): ReactNode {
  const [value, setValue] = useState("Old name");
  const [kept, setKept] = useState<readonly string[]>([]);
  const [drawn, setDrawn] = useState(true);
  land.write = setValue;
  return (
    <>
      {drawn ? (
        <InPlaceField
          kept={kept}
          label="Title"
          onKeptChange={setKept}
          onSave={(next) => writes.push(next)}
          value={value}
        />
      ) : null}
      <button onClick={() => setDrawn((was) => !was)} type="button">
        {drawn ? "Take it off" : "Draw it again"}
      </button>
    </>
  );
}

test("the reader's texts the page keeps outlive the field: drawn again, it shows them again", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(<PageHeldField land={land} writes={writes} />);
  const page = within(view.container);
  const notice = () => page.queryByRole("alert", { name: "Title, changed by another person" });
  await onFakeClock(async () => {
    await user.type(page.getByRole("textbox", { name: "Title" }), ", mine");
    act(() => land.write?.("Written elsewhere"));
  });
  expect(notice()).not.toBeNull();
  await user.click(page.getByRole("button", { name: "Take it off" }));
  expect(notice()).toBeNull();
  await user.click(page.getByRole("button", { name: "Draw it again" }));
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the field drawn again");
  expect(within(kept).getByText("Old name, mine")).toBeDefined();
  await user.click(within(kept).getByRole("button", { name: "Use my text" }));
  expect(writes).toEqual(["Old name, mine"]);
  expect((page.getByRole("textbox", { name: "Title" }) as HTMLInputElement).value).toBe(
    "Old name, mine"
  );
  expect(notice()).toBeNull();
  view.unmount();
  expect(writes).toEqual(["Old name, mine"]);
});

test("a required field the reader empties writes nothing, and left, shows the text it started from again", async () => {
  const user = userEvent.setup({ delay: null });
  const { field, land, notice, view, writes } = heldField(true);
  await onFakeClock(async () => {
    await user.clear(field);
    await jump(SAVE_AFTER_MS);
    // Nothing went, so nothing of the reader's stands when another person's text lands.
    land("Written elsewhere");
  });
  expect(writes).toEqual([]);
  expect(notice()).toBeNull();
  expect(field.value).toBe("Written elsewhere");
  await user.clear(field);
  await user.tab();
  expect(writes).toEqual([]);
  expect(field.value).toBe("Written elsewhere");
  view.unmount();
  expect(writes).toEqual([]);
});

/** The field as a page draws it that may give its notice a place of its own (`noticeAt`), as the
 *  priority's page does for the title standing in its heading. The field stands in one region and
 *  the place in another, so a row reads where the notice is drawn. */
function PlacedField({
  land,
  placed,
}: {
  land: { write?: (text: string) => void };
  placed: boolean;
}): ReactNode {
  const [value, setValue] = useState("Old name");
  const [place, setPlace] = useState<HTMLElement | null>(null);
  land.write = setValue;
  return (
    <>
      <section aria-label="Field">
        <InPlaceField
          label="Title"
          noticeAt={placed ? place : undefined}
          onSave={() => undefined}
          value={value}
        />
      </section>
      <section aria-label="Place for the notice" ref={setPlace} />
    </>
  );
}

/** `PlacedField` drawn, with the reader's typing and another person's text landing under it. */
async function placedField(placed: boolean) {
  const user = userEvent.setup({ delay: null });
  const land: { write?: (text: string) => void } = {};
  const view = render(<PlacedField land={land} placed={placed} />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" });
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    act(() => land.write?.("Written elsewhere"));
  });
  return {
    field,
    notice: page.getByRole("alert", { name: "Title, changed by another person" }),
    place: page.getByRole("region", { name: "Place for the notice" }),
    user,
    view,
  };
}

test("a field given a place for its notice draws it there, and names it as what describes the field", async () => {
  const { field, notice, place, user, view } = await placedField(true);
  expect(within(notice).getByText("Old name, mine")).toBeDefined();
  expect(notice.parentElement).toBe(place);
  expect(notice.id).not.toBe("");
  expect(field.getAttribute("aria-describedby")).toBe(notice.id);
  // Let go, the notice leaves its place empty, which a page may hide while it is, and the field
  // names nothing.
  await user.click(within(notice).getByRole("button", { name: "Keep their text" }));
  expect(place.childNodes).toHaveLength(0);
  expect(field.getAttribute("aria-describedby")).toBeNull();
  view.unmount();
});

test("a field given no place for its notice draws it beside itself, and names it all the same", async () => {
  const { field, notice, view } = await placedField(false);
  expect(within(notice).getByText("Old name, mine")).toBeDefined();
  expect(notice.parentElement).toBe(field.parentElement);
  expect(notice.id).not.toBe("");
  expect(field.getAttribute("aria-describedby")).toBe(notice.id);
  view.unmount();
});
