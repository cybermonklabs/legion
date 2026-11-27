import { expect, test } from "bun:test";

import { catalogueEntry, NO_PERSON_KEYS } from "./keymap-catalogue";
import { refusalOf } from "./keymap-refusal";

test("e for Snooze is refused, naming Dismiss, the action of the same page that has it", () => {
  const verdict = refusalOf("inbox.snooze", ["e"], NO_PERSON_KEYS);
  expect(verdict).toMatchObject({ kind: "refused", other: "inbox.dismiss" });
  expect(verdict.kind === "refused" && verdict.message).toContain("Dismiss, or Move to Inbox");
});

test("e for Snooze is taken once the person gave Dismiss another key", () => {
  expect(refusalOf("inbox.snooze", ["e"], { "inbox.dismiss": ["q"] })).toEqual({
    kind: "accepted",
    notices: [],
  });
});

test("g is refused, naming an action whose first press is g", () => {
  const verdict = refusalOf("inbox.snooze", ["g"], NO_PERSON_KEYS);
  expect(verdict.kind).toBe("refused");
  if (verdict.kind !== "refused" || verdict.other === undefined) throw new Error("no action named");
  const other = catalogueEntry(verdict.other);
  expect(other.keys.some((key) => key.startsWith("g "))).toBe(true);
  expect(verdict.message).toContain(other.label);
});

test("a key whose first press is another action's key is refused too", () => {
  // `l` is the Inbox's `Move the focus into the reading pane`, so `l x` could never be pressed.
  const verdict = refusalOf("inbox.snooze", ["l x"], NO_PERSON_KEYS);
  expect(verdict).toMatchObject({ kind: "refused", other: "inbox.read-pane" });
});

test("1 is refused: the digits keep their own actions", () => {
  expect(refusalOf("inbox.snooze", ["1"], NO_PERSON_KEYS).kind).toBe("refused");
  for (const key of ["Space", "Shift+Space", "Enter", "Escape", "Tab", "ArrowDown"]) {
    expect(refusalOf("inbox.snooze", [key], NO_PERSON_KEYS).kind, key).toBe("refused");
  }
  // The same keys with Alt are free: the document's Alt+Enter is one.
  expect(refusalOf("inbox.snooze", ["Alt+Enter"], NO_PERSON_KEYS).kind).toBe("accepted");
});

test("a key the browser keeps for itself is refused", () => {
  for (const key of ["$mod+w", "Control+t", "Meta+n", "$mod+Tab"]) {
    expect(refusalOf("global.create", [key], NO_PERSON_KEYS).kind, key).toBe("refused");
  }
});

test("an action whose keys all keep their own action takes no key", () => {
  expect(refusalOf("inbox.option", ["q"], NO_PERSON_KEYS).kind).toBe("refused");
  expect(refusalOf("inbox.option", [], NO_PERSON_KEYS).kind).toBe("accepted");
});

test("c is taken on the Inbox with the notice that Create issue does not work there", () => {
  expect(refusalOf("inbox.snooze", ["c"], NO_PERSON_KEYS)).toEqual({
    kind: "accepted",
    notices: ["Create issue does not work on the Inbox."],
  });
  // And the other way round: a global action taking a page's key.
  expect(refusalOf("global.create", ["e"], NO_PERSON_KEYS)).toMatchObject({
    kind: "accepted",
    notices: expect.arrayContaining(["Create issue does not work on the Inbox."]),
  });
});

test("a key of the issue page's document is refused on the issue page", () => {
  expect(refusalOf("issue.labels", ["Alt+Enter"], NO_PERSON_KEYS)).toMatchObject({
    kind: "refused",
    other: "document.open-link",
  });
  // A key a document's action may take, since it types nothing in a text field, meets the issue
  // page's key all the same.
  expect(refusalOf("document.open-link", ["Alt+F2"], { "issue.labels": ["Alt+F2"] })).toMatchObject(
    { kind: "refused", other: "issue.labels" }
  );
});

// Search and actions, Comment on selection and Open the link at the caret also fire while the
// reader types (`inEditable`), so a key of theirs that a text field takes as its own would be
// taken from every field, whether or not another action holds it.
test("an action that also works while the reader types takes no key a text field takes as its own", () => {
  for (const [action, key] of [
    ["global.search", "u"],
    ["global.search", "Backspace"],
    // No action holds these on the same page; the issue page's `s` does not fire in its fields.
    ["global.search", "s"],
    ["global.search", "Shift+U"],
    ["global.search", "Delete"],
    ["global.search", "Home"],
    // Option types a character with a letter on a Mac, and deletes a word with Backspace.
    ["global.search", "Alt+e"],
    ["global.search", "Alt+Backspace"],
    // Each press counts, the second of two too.
    ["global.search", "$mod+j x"],
    ["document.comment-on-selection", "m"],
    ["document.open-link", "l"],
  ] as const) {
    const verdict = refusalOf(action, [key], NO_PERSON_KEYS);
    expect(verdict, `${action} ${key}`).toMatchObject({ kind: "refused", other: undefined });
    expect(verdict.kind === "refused" && verdict.message, `${action} ${key}`).toContain(
      "also works while you type"
    );
  }
  for (const [action, key] of [
    ["global.search", "$mod+j"],
    ["global.search", "Alt+F2"],
    ["document.comment-on-selection", "$mod+Shift+m"],
    ["document.open-link", "Alt+Enter"],
  ] as const) {
    expect(refusalOf(action, [key], NO_PERSON_KEYS).kind, `${action} ${key}`).toBe("accepted");
  }
  // An action that fires only outside a text field takes a plain letter.
  expect(refusalOf("inbox.snooze", ["u"], NO_PERSON_KEYS).kind).toBe("accepted");
});

// A text field's own keys with Ctrl or Cmd - paste, copy, cut, undo, redo, select all, delete a
// word, move the caret - are its own with Shift or Alt too: Ctrl+Shift+Z redoes.
test("an action that also works while the reader types takes no Ctrl or Cmd key a text field takes as its own", () => {
  // Each key as the message draws it away from a Mac (`keyText`).
  for (const [key, drawn] of [
    ["$mod+v", "Ctrl+v"],
    ["$mod+z", "Ctrl+z"],
    ["$mod+Backspace", "Ctrl+Backspace"],
    ["$mod+ArrowLeft", "Ctrl+←"],
    ["$mod+Shift+Z", "Ctrl+Shift+Z"],
  ] as const) {
    expect(refusalOf("global.search", [key], NO_PERSON_KEYS, false), key).toEqual({
      kind: "refused",
      message: `${drawn} would be taken from every text field: Search and actions also works while you type, so its key needs Ctrl or Meta with a key other than V, C, X, Z, Y, A, Backspace, Delete, an arrow, Home or End, or Alt with a key that types nothing, such as Alt+Enter.`,
      other: undefined,
    });
  }
  // Search and actions' default.
  expect(refusalOf("global.search", ["$mod+k"], NO_PERSON_KEYS).kind).toBe("accepted");
});

// On a Mac `$mod` is Cmd, and a Mac's text field keeps Control with A, B, D, E, F, H, K, N, O, P,
// T, V or Y to move the caret and edit, whatever else the press holds: Control+E moves to the end
// of the line and Control+K deletes to it. Cmd+E is no such key.
test("on a Mac an action that also works while the reader types takes no Control key a Mac's text field takes as its own, and takes Cmd+E", () => {
  // Each key and modifier as the message draws it on a Mac (`keyText`).
  for (const [key, drawn] of [
    ["Control+e", "⌃+e"],
    ["Control+k", "⌃+k"],
    ["Control+Shift+a", "⌃+⇧+a"],
  ] as const) {
    expect(refusalOf("global.search", [key], NO_PERSON_KEYS, true), key).toEqual({
      kind: "refused",
      message: `${drawn} would be taken from every text field: Search and actions also works while you type, so its key needs ⌘ or ⌃ with a key other than V, C, X, Z, Y, A, Backspace, Delete, an arrow, Home or End, or ⌥ with a key that types nothing, such as ⌥+Enter. With ⌃, a Mac text field also uses B, D, E, F, H, K, O and P to move the cursor and edit.`,
      other: undefined,
    });
  }
  expect(refusalOf("global.search", ["$mod+e"], NO_PERSON_KEYS, true).kind).toBe("accepted");
  // Elsewhere Control is `$mod`, and Ctrl+E is a key no text field takes.
  for (const key of ["$mod+e", "Control+e"]) {
    expect(refusalOf("global.search", [key], NO_PERSON_KEYS, false).kind, key).toBe("accepted");
  }
});

/** What the check says of `key` for Search and actions, an action that also works while the
 *  reader types, on a Mac or elsewhere: its refusal, or nothing where it takes the key. */
function messageOf(key: string, isMac: boolean): string {
  const verdict = refusalOf("global.search", [key], NO_PERSON_KEYS, isMac);
  return verdict.kind === "refused" ? verdict.message : "";
}

/** The words of the refusal of a key a text field takes as its own (`actsInField`). */
const FIELD_REFUSAL = "would be taken from every text field";

// The message's list of a text field's own keys with Ctrl or Cmd is the check's own
// (`FIELD_COMMAND_KEYS`): each key whose `$mod` press the check refuses as a text field's is
// named, a letter in upper case and the four arrows once, as "an arrow", and the list names no
// other key. The row reads the table through the check, so a key added to the table is a key the
// message must name.
test("the refusal names each key it refuses as a text field's with Ctrl or Cmd, and no other", () => {
  const candidates = [
    ..."abcdefghijklmnopqrstuvwxyz0123456789",
    "ArrowDown",
    "ArrowLeft",
    "ArrowRight",
    "ArrowUp",
    "Backspace",
    "Delete",
    "End",
    "Enter",
    "Escape",
    "F2",
    "Home",
    "Insert",
    "PageDown",
    "PageUp",
    "Space",
    "Tab",
  ];
  const refused = candidates.filter((key) =>
    messageOf(`$mod+${key}`, false).includes(FIELD_REFUSAL)
  );
  const expected = [
    ...new Set(
      refused.map((key) =>
        key.startsWith("Arrow") ? "an arrow" : key.length === 1 ? key.toUpperCase() : key
      )
    ),
  ];
  // Today's table, which the probe reaches; a key added to the table joins it.
  expect(expected).toEqual(
    expect.arrayContaining([
      "A",
      "C",
      "V",
      "X",
      "Y",
      "Z",
      "an arrow",
      "Backspace",
      "Delete",
      "End",
      "Home",
    ])
  );

  const listed =
    /with a key other than (.+), or Alt with a key that types nothing/.exec(
      messageOf("$mod+v", false)
    )?.[1] ?? "";
  const named = listed.split(/, | or /);
  // The table holds each key in lower case, so a name is read without its case.
  const folded = (words: readonly string[]) => words.map((word) => word.toLowerCase()).sort();
  expect(folded(named), "the keys the message names").toEqual(folded(expected));
});

// The message's list of a Mac text field's own Control keys is the check's own: each letter
// whose Control press the check refuses on a Mac as a text field's (`MAC_FIELD_CONTROL_KEYS`),
// but for those it names already with every modifier (V, C, X, Z, Y, A), and no letter whose
// Control press it refuses first as the browser's own (`isBrowsers`: N and T among them).
test("on a Mac the refusal names each Control key it refuses as a Mac text field's, and none the browser keeps", () => {
  const letters = [..."abcdefghijklmnopqrstuvwxyz"];
  const commandLetters = letters.filter((letter) =>
    messageOf(`$mod+${letter}`, false).includes(FIELD_REFUSAL)
  );
  const macControlLetters = letters.filter(
    (letter) =>
      messageOf(`Control+${letter}`, true).includes(FIELD_REFUSAL) &&
      !commandLetters.includes(letter)
  );
  const browsers = letters.filter((letter) =>
    messageOf(`Control+${letter}`, true).endsWith("is the browser's own key.")
  );
  expect(commandLetters).toEqual(["a", "c", "v", "x", "y", "z"]);
  expect(browsers).toEqual(expect.arrayContaining(["n", "t"]));

  const listed =
    /a Mac text field also uses (.+) to move the cursor and edit\.$/.exec(
      messageOf("Control+e", true)
    )?.[1] ?? "";
  const named = listed.split(/, | and /).map((letter) => letter.toLowerCase());
  expect([...named].sort()).toEqual(macControlLetters);
  expect(named.filter((letter) => browsers.includes(letter))).toEqual([]);
});

// One press is one key, as the platform reads `$mod`: Search and actions' `$mod+k` is Cmd+K on a
// Mac and Ctrl+K elsewhere, so another action of the shell may take the other of the two.
test("Search and actions' $mod+k is Meta+k on a Mac and Control+k elsewhere, and the other is free", () => {
  for (const [isMac, held, free] of [
    [true, "Meta+k", "Control+k"],
    [false, "Control+k", "Meta+k"],
  ] as const) {
    expect(refusalOf("global.create", [held], NO_PERSON_KEYS, isMac), held).toMatchObject({
      kind: "refused",
      other: "global.search",
    });
    expect(refusalOf("global.create", [free], NO_PERSON_KEYS, isMac).kind, free).toBe("accepted");
  }
});

// The `y` that copies a focused link's address is a binding written out whole, registered on
// every page (`link-copy.ts`), which the registry reads as the first press of `y i`.
test("a key whose first press is the focused link's y is refused, though no action holds y", () => {
  const noY = { "board.copy-ref": [], "inbox.copy-ref": [], "issue.copy-turn-ref": [] };
  const verdict = refusalOf("global.go-inbox", ["y i"], noY);
  expect(verdict).toMatchObject({ kind: "refused", other: undefined });
  expect(verdict.kind === "refused" && verdict.message).toBe(
    "y, Copy the focused link's address, is the first press of y i."
  );
  // With an action's `y` the refusal names that action, and `y` alone stays free for one.
  const named = refusalOf("global.go-inbox", ["y i"], NO_PERSON_KEYS);
  expect(named.kind).toBe("refused");
  expect(["board.copy-ref", "inbox.copy-ref", "issue.copy-turn-ref"]).toContain(
    named.kind === "refused" ? (named.other ?? "") : ""
  );
  expect(refusalOf("inbox.copy-ref", ["y"], NO_PERSON_KEYS).kind).toBe("accepted");
});

test("the List's and the Board's own o are no duplicate, but both meet the project page's keys", () => {
  expect(refusalOf("project.open", ["o"], NO_PERSON_KEYS)).toEqual({
    kind: "accepted",
    notices: [],
  });
  expect(refusalOf("board.open", ["o"], NO_PERSON_KEYS)).toEqual({
    kind: "accepted",
    notices: [],
  });
  // The Board's `p` is free on the List, whose tab never mounts beside the Board.
  expect(refusalOf("project.open", ["p"], NO_PERSON_KEYS).kind).toBe("accepted");
  // The project page's own `v` is drawn above both.
  expect(refusalOf("board.open", ["v"], NO_PERSON_KEYS)).toMatchObject({
    kind: "refused",
    other: "project.toggle-view",
  });
  expect(refusalOf("project.open", ["v"], NO_PERSON_KEYS)).toMatchObject({
    kind: "refused",
    other: "project.toggle-view",
  });
});

test("the new issue page's keys meet its documents' and the shell's, and never today's issue page's own or the margin's", () => {
  // Shift+M is Toggle margin on every page but the new issue page, whose frame opens Details by it.
  for (const action of ["global.toggle-margin", "global.toggle-details"] as const) {
    expect(refusalOf(action, ["Shift+M"], NO_PERSON_KEYS), action).toEqual({
      kind: "accepted",
      notices: [],
    });
  }
  // Toggle margin is not drawn on the new page, so a key of its outline is free for it.
  expect(refusalOf("global.toggle-margin", ["z"], NO_PERSON_KEYS)).toEqual({
    kind: "accepted",
    notices: [],
  });
  // The outline's `j` and today's Next turn: the two pages are never drawn together.
  expect(refusalOf("issue.tree-next", ["j"], NO_PERSON_KEYS)).toEqual({
    kind: "accepted",
    notices: [],
  });
  // Details' key is in the `global` scope beside every other key of the shell there.
  expect(refusalOf("global.toggle-details", ["c"], NO_PERSON_KEYS)).toMatchObject({
    kind: "refused",
    other: "global.create",
  });
  // The new page draws its documents too.
  expect(refusalOf("issue.tree-cycle", ["Alt+Enter"], NO_PERSON_KEYS)).toMatchObject({
    kind: "refused",
    other: "document.open-link",
  });
  // A key of the shell the outline takes is the outline's on the new page, as on any page.
  expect(refusalOf("issue.tree-cycle", ["c"], NO_PERSON_KEYS)).toEqual({
    kind: "accepted",
    notices: ["Create issue does not work on the new issue page."],
  });
});

// The header's keys and the tab chords are drawn on both issue pages, so on the new page they
// meet the outline's keys as on today's page they meet that page's own.
test("on the new issue page an outline action is refused a header key, and a header action an outline key", () => {
  const outline = refusalOf("issue.tree-link", ["s"], NO_PERSON_KEYS);
  expect(outline).toMatchObject({ kind: "refused", other: "issue.status" });
  expect(outline.kind === "refused" && outline.message).toContain("Open the status menu");
  const header = refusalOf("issue.status", ["f"], NO_PERSON_KEYS);
  expect(header).toMatchObject({ kind: "refused", other: "issue.tree-link" });
  expect(header.kind === "refused" && header.message).toContain(
    "Move to the next link or control on the focused row"
  );
  // A key of today's page alone and a key of the outline alone still never meet.
  expect(refusalOf("issue.next-turn", ["f"], NO_PERSON_KEYS)).toEqual({
    kind: "accepted",
    notices: [],
  });
});

// INFRA-1196's decision 164: `a` approves on the header's approval chip on both pages, and on the
// new page it answers the question of a focused outline row, two `when`s that never hold at once.
test("a is Approve's and Answer's both on the new issue page, as is any key a person gives the two", () => {
  for (const action of ["issue.approve", "issue.tree-answer"] as const) {
    expect(refusalOf(action, ["a"], NO_PERSON_KEYS), action).toEqual({
      kind: "accepted",
      notices: [],
    });
  }
  expect(refusalOf("issue.tree-answer", ["Shift+A"], { "issue.approve": ["Shift+A"] })).toEqual({
    kind: "accepted",
    notices: [],
  });
  // The pair shares no other key of the new page, and neither takes the first press of the other.
  expect(refusalOf("issue.tree-answer", ["Shift+R"], NO_PERSON_KEYS)).toMatchObject({
    kind: "refused",
    other: "issue.request-changes",
  });
  expect(
    refusalOf("issue.tree-answer", ["Shift+A z"], { "issue.approve": ["Shift+A"] })
  ).toMatchObject({ kind: "refused", other: "issue.approve" });
});

test("a key listed twice, or a key that is the first press of another key of the action, is refused", () => {
  // `u` is no action's key, so each refusal below is the action's own keys'.
  expect(refusalOf("inbox.snooze", ["u", "u"], NO_PERSON_KEYS).kind).toBe("refused");
  expect(refusalOf("inbox.snooze", ["u", "u w"], NO_PERSON_KEYS).kind).toBe("refused");
  expect(refusalOf("inbox.snooze", ["u w", "u"], NO_PERSON_KEYS).kind).toBe("refused");
  expect(refusalOf("inbox.snooze", ["Shift++"], NO_PERSON_KEYS).kind).toBe("refused");
  // Two chords that share a first press are no prefix of each other, as `g i` and `g a` are not.
  expect(refusalOf("inbox.snooze", ["u w", "u e"], NO_PERSON_KEYS).kind).toBe("accepted");
});
