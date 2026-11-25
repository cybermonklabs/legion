import { expect, test } from "bun:test";
import { renderHook } from "@testing-library/react";

import { type LineRead, useLineRefusals } from "./useLineRefusals";

/**
 * The count that keys a failed-read line's alert (`useLineRefusals`), read draw by draw: each
 * case draws the hook over one line, or two, and draws it again with the lines' reads as they
 * then stand, as a view draws it again as its reads change.
 */

/** A read under the key whose hash is `hash`, refused `errorUpdateCount` times, and failing or
 *  not: a read that holds an answer is not failing, whatever its count. */
function lineRead(hash: string, errorUpdateCount: number, failing: boolean): LineRead {
  return { errorUpdateCount, failing, hash };
}

/** The hook over one line of `reads`, drawn again with each new set of reads a case gives. */
function drawLine(reads: readonly LineRead[]) {
  const hook = renderHook(({ line }) => useLineRefusals([line])[0], {
    initialProps: { line: reads },
  });
  return {
    count: () => hook.result.current.refusals,
    draw: (line: readonly LineRead[]) => hook.rerender({ line }),
    speaks: () => hook.result.current.speaks,
    stands: () => hook.result.current.stands,
    unmount: () => hook.unmount(),
  };
}

// The statements line stands on two reads, each refused once. In one draw the first answers and
// the second is refused again: the refusals of the failing reads come to two before and two
// after, so a count that summed them would not change, where the line's count rises by the new
// refusal.
test("a new refusal of one read while another answers in the same draw raises the count", () => {
  const line = drawLine([lineRead("coverage", 1, true), lineRead("blocks", 1, true)]);
  try {
    expect(line.count()).toBe(2);
    line.draw([lineRead("coverage", 1, false), lineRead("blocks", 2, true)]);
    expect(line.count(), "the new refusal of the blocks was not counted").toBe(3);
  } finally {
    line.unmount();
  }
});

// The count is kept during render, so a view drawn again with nothing changed - a render of its
// own, or one its parent asked for - counts no refusal twice.
test("the count stays the same on a second draw of the same state", () => {
  const reads = [lineRead("decisions", 2, true)];
  const line = drawLine(reads);
  try {
    expect(line.count()).toBe(2);
    line.draw(reads);
    expect(line.count(), "a draw of the same reads counted again").toBe(2);
    line.draw([lineRead("decisions", 2, true)]);
    expect(line.count(), "a draw of the same state counted again").toBe(2);
  } finally {
    line.unmount();
  }
});

// A read whose key changes is a query of its own: the spec's blocks once the issue names a spec of
// another id, refused once there too. Its refusal is new to the line, though its count is no
// higher than the one the read had under the key before, so the whole of it is added.
test("a failing read whose key changes adds its new query's whole count", () => {
  const line = drawLine([lineRead("coverage", 0, false), lineRead("spec-a", 1, true)]);
  try {
    expect(line.count()).toBe(1);
    line.draw([lineRead("coverage", 0, false), lineRead("spec-b", 1, true)]);
    expect(line.count(), "the refusal under the new key was not counted").toBe(2);
    line.draw([lineRead("coverage", 0, false), lineRead("spec-b", 1, true)]);
    expect(line.count(), "a draw under the same key counted its refusal again").toBe(2);
  } finally {
    line.unmount();
  }
});

// The statements line stands on the coverage, refused twice, while the spec's blocks are out. The
// coverage answers and no read is failing: the line stands no more, and keeps its count, which
// never falls. When the blocks are then refused, the line stands again and counts that one
// refusal on top of the coverage's from before.
test("a line that stops standing keeps its count, since the count never falls, and adds only the refusal it stands again for", () => {
  const line = drawLine([lineRead("coverage", 2, true), lineRead("blocks", 0, false)]);
  try {
    expect(line.count()).toBe(2);
    expect(line.stands()).toBe(true);
    line.draw([lineRead("coverage", 2, false), lineRead("blocks", 0, false)]);
    expect(line.stands(), "the line stood with no read failing").toBe(false);
    expect(line.count(), "the count fell as the coverage answered").toBe(2);
    line.draw([lineRead("coverage", 2, false), lineRead("blocks", 1, true)]);
    expect(line.stands(), "the line did not stand for the blocks' refusal").toBe(true);
    expect(line.count(), "the line did not add the blocks' refusal alone").toBe(3);
  } finally {
    line.unmount();
  }
});

// A read out with no refusal under its key stands no line: the names read of a set never read
// before, out on its first try. Once the line stands, it stays while such a read is out, as a
// read moved to a new set after a refusal is.
test("a line stands for a refusal and stays through a read out under a new key, but not for that read alone", () => {
  const line = drawLine([lineRead("set-a", 0, true)]);
  try {
    expect(line.stands(), "a read out with no refusal stood the line").toBe(false);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.stands()).toBe(true);
    line.draw([lineRead("set-b", 0, true)]);
    expect(line.stands(), "the line left while the read under a new key was out").toBe(true);
    expect(line.count()).toBe(1);
  } finally {
    line.unmount();
  }
});

// A read back on a key whose refusal the line counted holds that refusal still, and the line has
// said it: the names read moved to a second set and refused there too, then moved back to the
// first set, still refused once. Each refusal is counted once under its key, so the count stays.
test("a failing read back on a key whose refusal the line counted adds nothing", () => {
  const line = drawLine([lineRead("set-a", 1, true)]);
  try {
    expect(line.count()).toBe(1);
    line.draw([lineRead("set-b", 1, true)]);
    expect(line.count(), "the refusal under the second key was not counted").toBe(2);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.count(), "the first key's refusal was counted again").toBe(2);
    expect(line.stands()).toBe(true);
  } finally {
    line.unmount();
  }
});

// A line that stopped standing, as its read answered under another key, stands again where the
// read is back on a key whose refusal it counted: that key's query holds no answer and its
// refusal still, which the line has said, so it stands with its count unchanged.
test("a line that stands again on a key whose refusal it counted keeps its count", () => {
  const line = drawLine([lineRead("set-a", 1, true)]);
  try {
    expect(line.count()).toBe(1);
    line.draw([lineRead("set-b", 0, false)]);
    expect(line.stands(), "the line stood as its read answered").toBe(false);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.stands(), "the line did not stand again on the refused key").toBe(true);
    expect(line.count(), "the first key's refusal was counted again").toBe(1);
  } finally {
    line.unmount();
  }
});

// A query made afresh or reset counts from 0 again, and its answer is gone with it: a count
// below the one seen under its key is all new refusals.
test("a read whose count fell below the one seen under its key adds the whole of it", () => {
  const line = drawLine([lineRead("set-a", 3, true)]);
  try {
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.count(), "the refusal of the query made afresh was not counted").toBe(4);
  } finally {
    line.unmount();
  }
});

// A line speaks as it begins standing with a new refusal, and goes on speaking while it stands,
// a second draw of the same state included. A read out with no refusal stands no line, and a line
// that does not stand does not speak.
test("a line that begins standing with a new refusal speaks while it stands", () => {
  const line = drawLine([lineRead("set-a", 0, true)]);
  try {
    expect(line.speaks(), "a line that does not stand spoke").toBe(false);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.speaks(), "the line began standing with a new refusal and said nothing").toBe(true);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.speaks(), "a second draw of the same state took the line's voice").toBe(true);
    line.draw([lineRead("set-b", 0, true)]);
    expect(line.speaks(), "the line went quiet while a read under a new key was out").toBe(true);
  } finally {
    line.unmount();
  }
});

// A line that stopped standing and stands again on a key whose refusal it counted has said that
// refusal: it draws without speaking, on every draw while it stands with no new refusal.
test("a line that stands again with no new refusal does not speak", () => {
  const line = drawLine([lineRead("set-a", 1, true)]);
  try {
    expect(line.speaks()).toBe(true);
    line.draw([lineRead("set-b", 0, false)]);
    expect(line.stands()).toBe(false);
    expect(line.speaks(), "a line that does not stand spoke").toBe(false);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.stands()).toBe(true);
    expect(line.speaks(), "the line said again a refusal it had counted").toBe(false);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.speaks(), "a second draw of the same state gave the line its voice").toBe(false);
  } finally {
    line.unmount();
  }
});

// A line that stands quietly speaks from its next new refusal, and goes on speaking while it
// stands, as a line that began standing with it would.
test("a line that stands quietly speaks once it counts a new refusal, while it stands", () => {
  const line = drawLine([lineRead("set-a", 1, true)]);
  try {
    line.draw([lineRead("set-b", 0, false)]);
    line.draw([lineRead("set-a", 1, true)]);
    expect(line.speaks()).toBe(false);
    line.draw([lineRead("set-a", 2, true)]);
    expect(line.count()).toBe(2);
    expect(line.speaks(), "the line's new refusal was not said").toBe(true);
    line.draw([lineRead("set-b", 0, true)]);
    expect(line.speaks(), "the line went quiet while a read under a new key was out").toBe(true);
    line.draw([lineRead("set-b", 0, false)]);
    expect(line.speaks(), "a line that does not stand spoke").toBe(false);
  } finally {
    line.unmount();
  }
});

// Each line counts the refusals of its own reads. The statements line stands on the spec's
// blocks, refused once, while the results line's Verification document's blocks answer. The issue
// then names its spec as its Verification document too: the results line starts to read the key
// the statements line counted, has said none of its refusals, and counts the refusal as new. Both
// lines then stand on that one key, and each counts its next refusal and speaks.
test("two lines on one refused key each count its refusals, and a line that joins a key another line counted counts them as new", () => {
  const hook = renderHook((lines: readonly (readonly LineRead[])[]) => useLineRefusals(lines), {
    initialProps: [[lineRead("spec", 1, true)], [lineRead("verification", 0, false)]],
  });
  try {
    expect(hook.result.current).toEqual([
      { refusals: 1, speaks: true, stands: true },
      { refusals: 0, speaks: false, stands: false },
    ]);
    hook.rerender([[lineRead("spec", 1, true)], [lineRead("spec", 1, true)]]);
    expect(
      hook.result.current,
      "the line that joined a key another line counted did not count its refusal"
    ).toEqual([
      { refusals: 1, speaks: true, stands: true },
      { refusals: 1, speaks: true, stands: true },
    ]);
    hook.rerender([[lineRead("spec", 2, true)], [lineRead("spec", 2, true)]]);
    expect(hook.result.current, "a line on the shared key did not count its new refusal").toEqual([
      { refusals: 2, speaks: true, stands: true },
      { refusals: 2, speaks: true, stands: true },
    ]);
  } finally {
    hook.unmount();
  }
});
