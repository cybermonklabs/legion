import { abortOnReaderAct, claimUnacted } from "./reader-acts";

export async function copyText(text: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard !== undefined) {
    // The clipboard answers after a wait, and the fallback below takes the page's selection and,
    // in a browser, the focus, and gives both back as it ends. A reader who acts in that wait
    // (`READER_ACTS`) - a press in a field, and words typed there - has put the focus and the
    // caret where they want them, so a refusal then copies nothing and says so, and the caller
    // offers the words to copy by hand. The fallback's taking and giving back would blur and
    // focus the reader's field again, which ends what its focus holds open - an edit, a picker, a
    // word being composed. The press or the key that asked for the copy reached the window before
    // the guard is made, so it does not end it.
    const acted = abortOnReaderAct();
    try {
      await navigator.clipboard.writeText(text);
      acted.abort();
      return true;
    } catch {
      // Fall through to the legacy path, which also works on non-secure origins.
    }
    if (!claimUnacted(acted)) return false;
  }

  const giveBack = keepFocusAndSelection();
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.left = "-9999px";
  textarea.style.position = "fixed";
  textarea.style.top = "0";
  document.body.append(textarea);
  textarea.select();

  try {
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    textarea.remove();
    giveBack();
  }
}

/**
 * Reads where the focus, a field's caret and the page's selection are, and returns what puts them
 * back there. The selection goes back first: focusing a field moves the page's selection into it,
 * so a selection set after the focus would take the field's caret from it.
 */
function keepFocusAndSelection(): () => void {
  const focused = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const field =
    focused instanceof HTMLInputElement || focused instanceof HTMLTextAreaElement ? focused : null;
  const start = field?.selectionStart ?? null;
  const end = field?.selectionEnd ?? null;
  const direction = field?.selectionDirection ?? undefined;
  const selection = window.getSelection();
  const ranges =
    selection === null
      ? []
      : Array.from({ length: selection.rangeCount }, (_, index) =>
          selection.getRangeAt(index).cloneRange()
        );
  return () => {
    if (
      selection !== null &&
      ranges.every((range) => range.startContainer.isConnected && range.endContainer.isConnected)
    ) {
      selection.removeAllRanges();
      for (const range of ranges) selection.addRange(range);
    }
    if (!focused?.isConnected) return;
    focused.focus({ preventScroll: true });
    if (field === null || start === null || end === null) return;
    try {
      field.setSelectionRange(start, end, direction);
    } catch {
      // A field whose type a handler of the copy changed to one with no caret (a number, a
      // checkbox) refuses a caret, and the copy is still made.
    }
  };
}
