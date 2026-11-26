import { abortOnReaderAct } from "./reader-acts";

export async function copyText(text: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard !== undefined) {
    // The clipboard answers after a wait, and the fallback below takes the page's selection and,
    // in a browser, the focus, then drops the focus to the page's body. A reader who acts in that
    // wait (`READER_ACTS`) - a press in a field, and words typed there - has put the focus and the
    // caret where they want them, so a refusal then copies nothing and says so, and the caller
    // offers the words to copy by hand. Putting the focus back after the fallback would blur and
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
    if (acted.signal.aborted) return false;
    acted.abort();
  }

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
  }
}
