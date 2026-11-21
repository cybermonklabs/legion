import { expect, jest, test } from "bun:test";
import { act, render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { jump, realClock } from "../../__tests__/fake-clock";
import { userOnFakeClock } from "../../__tests__/user";
import { InPlaceField } from "./InPlaceField";
import { SAVE_AFTER_MS } from "./useInPlaceDraft";

/**
 * The field as a page holds it: `value` is the text the server answered with, and it moves only
 * when something writes it — the reader's own write coming back, or another writer's. The two
 * buttons stand for those two moments, so a test says when each happens rather than waiting on
 * a mutation it cannot see. Every text the field wrote is recorded in `saves`, in order.
 */
function Field({ saves = [] }: { saves?: string[] }): ReactNode {
  const [value, setValue] = useState("Old name");
  const [sent, setSent] = useState<string>();
  return (
    <>
      <InPlaceField
        label="Title"
        onSave={(next) => {
          saves.push(next);
          setSent(next);
          return Promise.resolve();
        }}
        value={value}
      />
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

/** A field whose value nothing moves, with every text it wrote recorded in `saves`, in order. */
function recordingField() {
  const saves: string[] = [];
  const view = render(
    <InPlaceField
      label="Title"
      onSave={(next) => {
        saves.push(next);
        return Promise.resolve();
      }}
      value="Old"
    />
  );
  return { field: within(view.container).getByRole("textbox", { name: "Title" }), saves, view };
}

// Enter leaves the field, and leaving is what writes: the same text sent twice would be a
// second write of a priority nobody changed again.
test("Enter writes what the reader typed once", async () => {
  const user = userEvent.setup();
  const { field, saves, view } = recordingField();
  await user.type(field, " new{Enter}");
  expect(saves).toEqual(["Old new"]);
  view.unmount();
});

test("leaving the field by Tab writes what the reader typed once", async () => {
  const user = userEvent.setup();
  const { field, saves, view } = recordingField();
  await user.type(field, " new");
  await user.tab();
  expect(saves).toEqual(["Old new"]);
  view.unmount();
});

// The rest writes the text, and the write is a round trip: `value` is still the old text until
// its answer lands. A reader who leaves inside that trip has written nothing new, so leaving
// must not send the same text again. The clock is the test's, so the rest is crossed rather than
// waited out.
test("Enter after the typing rested writes what the reader typed once", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, saves, view } = recordingField();
  try {
    await user.type(field, " new");
    await jump(SAVE_AFTER_MS + 1);
    expect(saves).toEqual(["Old new"]);
    await user.keyboard("{Enter}");
    expect(saves).toEqual(["Old new"]);
  } finally {
    realClock();
    view.unmount();
  }
});

test("leaving by Tab after the typing rested writes what the reader typed once", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, saves, view } = recordingField();
  try {
    await user.type(field, " new");
    await jump(SAVE_AFTER_MS + 1);
    expect(saves).toEqual(["Old new"]);
    await user.tab();
    expect(saves).toEqual(["Old new"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// Once the write lands the text is the server's again, so what the reader types next is a new
// text, written once however many times they rest and leave.
test("after the write lands, the next text typed is written once", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const saves: string[] = [];
  const view = render(<Field saves={saves} />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" });
  try {
    await user.type(field, " one");
    await jump(SAVE_AFTER_MS + 1);
    await user.click(page.getByRole("button", { name: "Land the write" }));
    await user.type(field, " two");
    await jump(SAVE_AFTER_MS + 1);
    await user.tab();
    expect(saves).toEqual(["Old name one", "Old name one two"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// While the write is out the text the server will hold is the one the rest wrote, not `value`:
// a reader who deletes back to the old text before the answer lands and leaves is changing the
// text again, and that text is written.
test("typing back to the old text while the write is out writes the old text", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const saves: string[] = [];
  const view = render(<Field saves={saves} />);
  const field = within(view.container).getByRole("textbox", { name: "Title" });
  try {
    await user.type(field, " new");
    await jump(SAVE_AFTER_MS + 1);
    expect(saves).toEqual(["Old name new"]);
    await user.keyboard("{Backspace}".repeat(" new".length));
    expect((field as HTMLInputElement).value).toBe("Old name");
    await user.tab();
    expect(saves).toEqual(["Old name new", "Old name"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// What the field last wrote stops counting once `value` moves: another writer may have changed
// the text since, and a reader who writes their text back is changing it again.
test("a text written before the value moved is written again when the reader writes it back", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const saves: string[] = [];
  const view = render(<Field saves={saves} />);
  const page = within(view.container);
  const field = page.getByRole("textbox", { name: "Title" });
  try {
    await user.type(field, " one");
    await jump(SAVE_AFTER_MS + 1);
    await user.click(page.getByRole("button", { name: "Land the write" }));
    await user.click(page.getByRole("button", { name: "Write it elsewhere" }));
    expect((field as HTMLInputElement).value).toBe("Written elsewhere");
    await user.clear(field);
    await user.type(field, "Old name one");
    await user.tab();
    expect(saves).toEqual(["Old name one", "Old name one"]);
  } finally {
    realClock();
    view.unmount();
  }
});

/**
 * A field over a server the test answers by hand, from outside the field: a button would take
 * the focus out of it, and most answers these rows are about land while the reader holds it.
 * Every text the field sends is recorded in `saves`, in order, and stays out until the test
 * answers it (`answer`, which lands the oldest write of a text still out, as writes to one
 * priority land in press order) or refuses it (`refuse`). A read brings a text back as `value`
 * (`lands`) - a text the field sent, or another writer's - and is the test's to make, since an
 * answer moves no read of its own. The rows below are the transitions `useInPlaceDraft` names,
 * one row each.
 */
function heldServer() {
  const saves: string[] = [];
  const out: {
    readonly land: () => void;
    readonly refuse: (refusal: Error) => void;
    readonly text: string;
  }[] = [];
  let read: (text: string) => void = () => {
    throw new Error("the field has not rendered");
  };
  const ServerField = (): ReactNode => {
    const [value, setValue] = useState("Old");
    read = setValue;
    return (
      <InPlaceField
        label="Title"
        onSave={(next) => {
          saves.push(next);
          const write = Promise.withResolvers<void>();
          out.push({ land: write.resolve, refuse: write.reject, text: next });
          return write.promise;
        }}
        value={value}
      />
    );
  };
  const view = render(<ServerField />);
  return {
    /** Lands the oldest write of `text` still out. */
    answer: (text: string) =>
      act(async () => {
        const at = out.map((write) => write.text).indexOf(text);
        const [write] = at === -1 ? [] : out.splice(at, 1);
        if (write === undefined) throw new Error(`no write of ${text} is out`);
        write.land();
      }),
    field: within(view.container).getByRole<HTMLInputElement>("textbox", { name: "Title" }),
    lands: (text: string) => act(() => read(text)),
    /** Refuses the newest write of `text` still out. */
    refuse: (text: string) =>
      act(async () => {
        const at = out.map((write) => write.text).lastIndexOf(text);
        const [write] = at === -1 ? [] : out.splice(at, 1);
        if (write === undefined) throw new Error(`no write of ${text} is out`);
        write.refuse(new Error("refused"));
      }),
    saves,
    view,
  };
}

// Typing: the draft is the reader's text and nothing is sent, and each keystroke starts the
// rest's wait again. The reader pauses for half the wait, types on, and pauses past the end of
// the first wait: nothing is sent until the wait after the last keystroke is over.
test("typing sends nothing until the typing rests", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, saves, view } = heldServer();
  try {
    await user.type(field, " new");
    await jump(SAVE_AFTER_MS / 2);
    expect(saves).toEqual([]);
    await user.type(field, "er");
    await jump(SAVE_AFTER_MS / 2 + 10);
    expect(field.value).toBe("Old newer");
    expect(saves).toEqual([]);
    await jump(SAVE_AFTER_MS / 2);
    expect(saves).toEqual(["Old newer"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// Sent and pending: a text the server is about to hold is not sent again - the newest text out,
// or `value` while none is - and any other text is, the old one included.
test("a text out, or the value while none is, is not sent again, and any other text is", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, saves, view } = heldServer();
  try {
    await user.click(field);
    await user.tab();
    expect(saves).toEqual([]);
    await user.type(field, " new");
    await jump(SAVE_AFTER_MS + 1);
    await user.tab();
    expect(saves).toEqual(["Old new"]);
    await user.type(field, "{Backspace}".repeat(" new".length));
    await user.tab();
    expect(saves).toEqual(["Old new", "Old"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// Refused: the send leaves what is out and the draft stays the reader's, so the next leave sends
// it again, by Enter as by Tab.
test("a refused write is sent again by the next Enter", async () => {
  const user = userEvent.setup();
  const { field, refuse, saves, view } = heldServer();
  try {
    await user.type(field, " new{Enter}");
    expect(saves).toEqual(["Old new"]);
    await refuse("Old new");
    expect(field.value).toBe("Old new");
    await user.type(field, "{Enter}");
    expect(saves).toEqual(["Old new", "Old new"]);
  } finally {
    view.unmount();
  }
});

test("a refused write is sent again by the next Tab", async () => {
  const user = userEvent.setup();
  const { field, refuse, saves, view } = heldServer();
  try {
    await user.type(field, " new");
    await user.tab();
    expect(saves).toEqual(["Old new"]);
    await refuse("Old new");
    await user.click(field);
    await user.tab();
    expect(saves).toEqual(["Old new", "Old new"]);
  } finally {
    view.unmount();
  }
});

// A refusal takes back that send alone, wherever it stands: a newer text still out is still what
// the server is about to hold, and leaving sends nothing.
test("a refused earlier write leaves a later one out", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, refuse, saves, view } = heldServer();
  try {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS + 1);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS + 1);
    await refuse("Old A");
    await user.tab();
    expect(saves).toEqual(["Old A", "Old AB"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// `value` reaching a text out: that send's answer is in, and the sends after it are still out.
// The read after the first of two writes lands while the reader still holds the field, and the
// Enter after it sends nothing, since the second text is what the server is about to hold.
test("the answer to an earlier write landing under the reader sends the later text once", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, lands, saves, view } = heldServer();
  try {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS + 1);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS + 1);
    expect(saves).toEqual(["Old A", "Old AB"]);
    await lands("Old A");
    await user.keyboard("{Enter}");
    expect(saves).toEqual(["Old A", "Old AB"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// Every send before the one `value` reached has its answer in too: the server holds the later
// text, so a reader who types back to the earlier one is changing it, and that text is written.
test("an answer to a later write takes every earlier write with it", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, lands, saves, view } = heldServer();
  try {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS + 1);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS + 1);
    await lands("Old AB");
    await user.keyboard("{Backspace}");
    await user.tab();
    expect(saves).toEqual(["Old A", "Old AB", "Old A"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// A stale read landing while a newer write is out: the reader has left the field, and the read
// after their first write brings that text back. The field keeps the newer text it holds rather
// than showing the old one, and leaving it again sends nothing.
test("a stale read landing while a newer write is out leaves the newer text in the field", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, lands, saves, view } = heldServer();
  try {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS + 1);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS + 1);
    await user.tab();
    await lands("Old A");
    expect(field.value).toBe("Old AB");
    await user.click(field);
    await user.tab();
    expect(saves).toEqual(["Old A", "Old AB"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// A read answers the earliest send of its text: writes to one priority land in press order, so
// the second send of `Old A` cannot land before `Old AB` has. The reader sends `Old A`, `Old AB`
// and `Old A` again; the first lands and is read, then `Old AB` lands and is read. The second
// `Old A` is still out, so the Enter after it sends nothing more.
test("a read of a text sent twice answers its earliest send, leaving the later sends out", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { answer, field, lands, saves, view } = heldServer();
  try {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS + 1);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS + 1);
    await user.keyboard("{Backspace}");
    await jump(SAVE_AFTER_MS + 1);
    expect(saves).toEqual(["Old A", "Old AB", "Old A"]);
    await answer("Old A");
    await lands("Old A");
    await answer("Old AB");
    await lands("Old AB");
    await user.keyboard("{Enter}");
    expect(saves).toEqual(["Old A", "Old AB", "Old A"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// A landed send leaves what is out on the next move of `value`, so two answers read out of
// order leave nothing behind: both writes land, and the read brings the earlier text. The
// later text is no longer what Dispatch is about to hold, so the reader's Enter writes it again.
test("two landed writes read out of order have the reader's later text written again", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { answer, field, lands, saves, view } = heldServer();
  try {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS + 1);
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS + 1);
    await answer("Old A");
    await answer("Old AB");
    await lands("Old A");
    await user.keyboard("{Enter}");
    expect(saves).toEqual(["Old A", "Old AB", "Old AB"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// A write of the text Dispatch already holds moves no read: the reader sends `Old X`, types back
// to `Old`, which is sent too, and leaves; both land and `value` stays `Old`. When another writer
// then picks `Old X`, nothing the field sent is still about to be held, so the field shows it.
test("a landed write of the text Dispatch held leaves nothing behind when the value next moves", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { answer, field, lands, saves, view } = heldServer();
  try {
    await user.type(field, " X");
    await jump(SAVE_AFTER_MS + 1);
    await user.keyboard("{Backspace}{Backspace}");
    await jump(SAVE_AFTER_MS + 1);
    await user.tab();
    expect(saves).toEqual(["Old X", "Old"]);
    await answer("Old X");
    await answer("Old");
    await lands("Old X");
    expect(field.value).toBe("Old X");
    await user.click(field);
    await user.tab();
    expect(saves).toEqual(["Old X", "Old"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// `value` moving to a text the field never sent: the server holds another writer's text now, so
// nothing the field sent is still about to be held, and the reader's text is written again.
test("another writer's text landing while a write is out has the reader's text written again", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { field, lands, saves, view } = heldServer();
  try {
    await user.type(field, " one");
    await jump(SAVE_AFTER_MS + 1);
    await lands("Written elsewhere");
    expect(field.value).toBe("Old one");
    await user.tab();
    expect(saves).toEqual(["Old one", "Old one"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// A Retry of a refused write from the pending-writes indicator is the reader's own new write of
// that text, made at the back of the line, so it lands after the newer text the field sent
// since. The field sent nothing that Retry carries: its landing reads as another writer's text,
// and the field, which the reader is not holding, shows what Dispatch holds.
test("a refused text a Retry from the indicator lands after a newer one reads as another writer's text", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { answer, field, lands, refuse, saves, view } = heldServer();
  try {
    await user.type(field, " A");
    await jump(SAVE_AFTER_MS + 1);
    await refuse("Old A");
    await user.type(field, "B");
    await jump(SAVE_AFTER_MS + 1);
    await user.tab();
    expect(saves).toEqual(["Old A", "Old AB"]);
    await answer("Old AB");
    await lands("Old AB");
    // The Retry's landing, read: no write of the field's carried it.
    await lands("Old A");
    expect(field.value).toBe("Old A");
    await user.click(field);
    await user.tab();
    expect(saves).toEqual(["Old A", "Old AB"]);
  } finally {
    realClock();
    view.unmount();
  }
});
