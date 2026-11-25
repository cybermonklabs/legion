import { expect, jest, test } from "bun:test";
import { act, render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import {
  BeforeEffects,
  type StepBeforeEffects,
  stepBeforeEffects,
} from "../../__tests__/before-effects";
import { jump, onFakeClock, settle } from "../../__tests__/fake-clock";
import { COMPOSING, KEY_CODE_229, pressInputMethodKey } from "../../__tests__/input-method-keys";
import { userOnFakeClock } from "../../__tests__/user";
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

// The rest and the leaving are each a moment of their own here: the clock is held, so the rest
// comes when the row moves it past `SAVE_AFTER_MS` and never between two keys of the typing. The
// server's text stays the old one throughout, as it does while the rested write's answer is out.
test("a text written when the typing rested is not written again when the field is left", async () => {
  await onFakeClock(async () => {
    const user = userOnFakeClock();
    const writes: string[] = [];
    const view = render(
      <InPlaceField label="Title" onSave={(next) => void writes.push(next)} value="Old name" />
    );
    const field = within(view.container).getByRole("textbox", { name: "Title" });
    await user.clear(field);
    await user.type(field, "New name");
    expect(writes).toEqual([]);
    await jump(SAVE_AFTER_MS);
    expect(writes).toEqual(["New name"]);
    await user.tab();
    expect(writes).toEqual(["New name"]);
    view.unmount();
  });
});

test("a text changed after the typing rested is written when the field is left", async () => {
  await onFakeClock(async () => {
    const user = userOnFakeClock();
    const writes: string[] = [];
    const view = render(
      <InPlaceField label="Title" onSave={(next) => void writes.push(next)} value="Old name" />
    );
    const field = within(view.container).getByRole("textbox", { name: "Title" });
    await user.clear(field);
    await user.type(field, "New");
    await jump(SAVE_AFTER_MS);
    await user.type(field, " name");
    await user.tab();
    expect(writes).toEqual(["New", "New name"]);
    view.unmount();
  });
});

/** The field as a page holds it while another person's write lands: every write the field sends
 *  is kept in `writes`, in `froms`, where given, the server's text the field last took in, which a
 *  priority's page names to Dispatch (`expected`), and in `madeOn`, where given, the text each
 *  write's edit was made on (`onSave`'s `on`). Where `answers` is given, the page answers each
 *  write with whether it landed, as a title's page does, once the test calls that write's answer
 *  there; else it answers none, as the owner's page does. The field is drawn with `start`, and
 *  `land.write` stands for the server's answer moving `value` under the field, as a refresh of the
 *  row does, without taking the focus from it; `step`, where given, runs in the commit that draws
 *  it, after the field's render and before its effects (`BeforeEffects`). */
function HeldField({
  answers,
  froms,
  land,
  madeOn,
  required,
  start = "Old name",
  step,
  writes,
}: {
  answers?: ((landed: boolean) => void)[];
  froms?: string[];
  land: { write?: (text: string) => void };
  madeOn?: string[];
  required?: boolean;
  start?: string;
  step?: StepBeforeEffects;
  writes: string[];
}): ReactNode {
  const [value, setValue] = useState(start);
  land.write = setValue;
  return (
    <>
      <InPlaceField
        label="Title"
        onSave={(next, from, on) => {
          writes.push(next);
          froms?.push(from);
          madeOn?.push(on);
          if (answers === undefined) return;
          const answer = Promise.withResolvers<boolean>();
          answers.push(answer.resolve);
          return answer.promise;
        }}
        required={required}
        value={value}
      />
      {step === undefined ? null : <BeforeEffects step={step} />}
    </>
  );
}

/** The page's answer to the `at`th write `HeldField` sent, of those whose answers it keeps in
 *  `answers`: landed or not, in an act that lets the field follow it. */
function answerAt(
  answers: readonly ((landed: boolean) => void)[],
  at: number,
  landed: boolean
): Promise<void> {
  return act(async () => {
    answers[at]?.(landed);
    await settle();
  });
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
  ["isComposing", COMPOSING],
  ["keyCode 229", KEY_CODE_229],
] as const)("the Enter that ends a composition, marked by %s, neither writes nor leaves the field, and a plain one does both", async (_mark, form) => {
  const user = userEvent.setup({ delay: null });
  const { field, view, writes } = heldField();
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    pressInputMethodKey(field, "Enter", form);
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
          onSave={(next) => void writes.push(next)}
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

// The page draws another person's title while the reader's typing waits, and the typing comes to
// rest after that render and before the field's effects take the title in. The write names the
// text the field started from - never the page's read, which holds the other person's title by
// then - so Dispatch refuses it rather than writing the reader's edit over theirs, and the field
// takes their title in and keeps the reader's beside it, as it does where theirs arrives first.
test("a write made once the page drew another person's title, and before the field took it in, names the text the field started from", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const froms: string[] = [];
  const land: { write?: (text: string) => void } = {};
  const { beforeEffects, step } = stepBeforeEffects();
  const view = render(<HeldField froms={froms} land={land} step={step} writes={writes} />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    beforeEffects(() => jest.advanceTimersByTime(SAVE_AFTER_MS));
    act(() => land.write?.("Written elsewhere"));
    await jump(SAVE_AFTER_MS);
  });
  expect(writes).toEqual(["Old name, mine"]);
  expect(froms).toEqual(["Old name"]);
  expect(field.value).toBe("Written elsewhere");
  const notice = page.getByRole("alert", { name: "Title, changed by another person" });
  expect(within(notice).getByText("Old name, mine")).toBeDefined();
  // Leaving the field writes nothing more: it holds the other person's title.
  await user.tab();
  view.unmount();
  expect(writes).toEqual(["Old name, mine"]);
  expect(froms).toEqual(["Old name"]);
});

// The page's answer to a write is that write's own, never another write's of the same text. The
// reader's "Old name, mine" is out when another person's title arrives; Use my text writes it
// again, and the reader types on and rests, which writes once more. Then the first write is
// refused, as another person's title stood: Use my text's write is still out, and its landing is
// the reader's own text coming back, with no notice and their typing left alone.
test("the answer to an earlier write of the same text leaves Use my text's write out, and that write's landing is the reader's own", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const answers: ((landed: boolean) => void)[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(<HeldField answers={answers} land={land} writes={writes} />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  const notice = () => page.queryByRole("alert", { name: "Title, changed by another person" });
  await onFakeClock(async () => {
    await user.type(field, ", mine");
    await jump(SAVE_AFTER_MS);
    act(() => land.write?.("Written elsewhere"));
  });
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the field");
  await user.click(within(kept).getByRole("button", { name: "Use my text" }));
  await onFakeClock(async () => {
    await user.type(field, ", more");
    await jump(SAVE_AFTER_MS);
  });
  expect(writes).toEqual(["Old name, mine", "Old name, mine", "Old name, mine, more"]);
  await answerAt(answers, 0, false);
  act(() => land.write?.("Old name, mine"));
  expect(notice()).toBeNull();
  expect(field.value).toBe("Old name, mine, more");
  view.unmount();
});

// A page that answers no write - the owner's - never says that one of the field's writes ended.
// The reader writes "Carol" over "Alice", and another person's "Bob" arrives; they write "Carol"
// again, and while that write is out, use the "Carol" kept beside the field. That write is on its
// way, so nothing more is sent. It lands, and then another person's "Dave" arrives: nothing of the
// reader's is out, so their text is not kept beside it.
test("Use my text where the field's own write of that text is still out sends nothing more, and a later text arrives with no notice", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(<HeldField land={land} start="Alice" writes={writes} />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  const notice = () => page.queryByRole("alert", { name: "Title, changed by another person" });
  await onFakeClock(async () => {
    await user.clear(field);
    await user.type(field, "Carol");
    await jump(SAVE_AFTER_MS);
    act(() => land.write?.("Bob"));
    await user.clear(field);
    await user.type(field, "Carol");
    await jump(SAVE_AFTER_MS);
  });
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the field");
  await user.click(within(kept).getByRole("button", { name: "Use my text" }));
  expect(notice()).toBeNull();
  act(() => land.write?.("Carol"));
  act(() => land.write?.("Dave"));
  expect(notice()).toBeNull();
  expect(field.value).toBe("Dave");
  view.unmount();
  expect(writes).toEqual(["Carol", "Carol"]);
});

// "A" over "Old" is out and "AB" over "A" waits behind it. The reader discards "AB" on the shell's
// line, and the page answers that it did not land; "A" lands, and a read shows it. The field still
// holds "AB", which the reader discarded: leaving the field, or the field leaving the page, writes
// nothing.
test("a write that ended without landing is not written again once a read shows the write before it", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const answers: ((landed: boolean) => void)[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(<HeldField answers={answers} land={land} start="Old" writes={writes} />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  await onFakeClock(async () => {
    await user.clear(field);
    await user.type(field, "A");
    await jump(SAVE_AFTER_MS);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS);
  });
  expect(writes).toEqual(["A", "AB"]);
  await answerAt(answers, 1, false);
  await answerAt(answers, 0, true);
  act(() => land.write?.("A"));
  await user.tab();
  expect(writes).toEqual(["A", "AB"]);
  expect(field.value).toBe("AB");
  expect(page.queryByRole("alert", { name: "Title, changed by another person" })).toBeNull();
  view.unmount();
  expect(writes).toEqual(["A", "AB"]);
});

// Each write hands on the text the field held as its edit began (`onSave`'s `on`): the text it
// was drawn with; its own last write, though that did not land; the other person's text it took
// in; for Use my text that writes the field's last write again, the text that write was made on,
// since it is that same edit; and, drawn again, the server's text once more, whatever the field
// before it wrote. A newer write lets a stopped one go only where it was made on that one's text,
// or writes that very text again (`priorityTextWrite.outdates`).
test("each write hands on the text the field held as its edit began, and a field drawn again starts from the server's", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const madeOn: string[] = [];
  const answers: ((landed: boolean) => void)[] = [];
  const land: { write?: (text: string) => void } = {};
  const held = (
    <HeldField answers={answers} land={land} madeOn={madeOn} start="Old" writes={writes} />
  );
  const view = render(held);
  const field = () => within(view.container).getByRole("textbox", { name: "Title" });
  await onFakeClock(async () => {
    await user.clear(field());
    await user.type(field(), "A");
    await jump(SAVE_AFTER_MS);
  });
  // "A" is stopped: the page answers it did not land, and the reader writes on from it.
  await answerAt(answers, 0, false);
  await onFakeClock(async () => {
    await user.type(field(), "B");
    await jump(SAVE_AFTER_MS);
  });
  await answerAt(answers, 1, false);
  // Another person's "Bob" arrives with nothing of hers out: the field takes it in.
  act(() => land.write?.("Bob"));
  await onFakeClock(async () => {
    await user.type(field(), ", C");
    await jump(SAVE_AFTER_MS);
  });
  // "Dave" arrives while "Bob, C" is out: it is kept beside the field, and she uses it, which
  // writes her last write again.
  act(() => land.write?.("Dave"));
  const notice = within(view.container).getByRole("alert", {
    name: "Title, changed by another person",
  });
  await user.click(within(notice).getByRole("button", { name: "Use my text" }));
  view.unmount();
  const again = render(held);
  await onFakeClock(async () => {
    await user.type(within(again.container).getByRole("textbox", { name: "Title" }), "D");
    await jump(SAVE_AFTER_MS);
  });
  again.unmount();
  expect(writes).toEqual(["A", "AB", "Bob, C", "Bob, C", "OldD"]);
  expect(madeOn).toEqual(["Old", "A", "Bob", "Bob", "Old"]);
});

// Use my text of typing the field never wrote is a new edit, made on the text the field held as
// she used it: the other person's, which it took in as it kept hers.
test("Use my text of typing the field never wrote is made on the text the field took in", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const madeOn: string[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(<HeldField land={land} madeOn={madeOn} start="Old" writes={writes} />);
  await onFakeClock(async () => {
    await user.type(within(view.container).getByRole("textbox", { name: "Title" }), ", mine");
    act(() => land.write?.("Bob"));
  });
  const notice = within(view.container).getByRole("alert", {
    name: "Title, changed by another person",
  });
  await user.click(within(notice).getByRole("button", { name: "Use my text" }));
  view.unmount();
  expect(writes).toEqual(["Old, mine"]);
  expect(madeOn).toEqual(["Bob"]);
});

// The text an edit was made on is the field's own, apart from what it compares the reader's text
// with (`base`), which a read moves under typing she has not written. "A", made on "Old", lands,
// and the read that shows it is slow; "AB", made on "A", ends without landing (she stopped it).
// She types on, and the read answers "A" while her typing waits: the field compares with "A" from
// then on, but her typing was made on "AB", and its write says so.
test("a read that answers an earlier write while the reader's typing waits leaves that typing made on the text it began on", async () => {
  const user = userEvent.setup({ delay: null });
  const writes: string[] = [];
  const madeOn: string[] = [];
  const answers: ((landed: boolean) => void)[] = [];
  const land: { write?: (text: string) => void } = {};
  const view = render(
    <HeldField answers={answers} land={land} madeOn={madeOn} start="Old" writes={writes} />
  );
  const field = within(view.container).getByRole("textbox", { name: "Title" });
  await onFakeClock(async () => {
    await user.clear(field);
    await user.type(field, "A");
    await jump(SAVE_AFTER_MS);
  });
  await answerAt(answers, 0, true);
  await onFakeClock(async () => {
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS);
  });
  await answerAt(answers, 1, false);
  await onFakeClock(async () => {
    await user.type(field, "C");
    act(() => land.write?.("A"));
    await jump(SAVE_AFTER_MS);
  });
  view.unmount();
  expect(writes).toEqual(["A", "AB", "ABC"]);
  expect(madeOn).toEqual(["Old", "A", "AB"]);
});
