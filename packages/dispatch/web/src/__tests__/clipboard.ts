/** Makes every copy fail, as a browser that refuses both routes does: the clipboard rejects the
 *  write, and the legacy fallback (`document.execCommand`) copies nothing. Returns what gives the
 *  page its own clipboard and `execCommand` back, for the row's `finally`. */
export function stubFailedCopy(): () => void {
  const clipboard = navigator.clipboard;
  const execCommand = document.execCommand;
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: () => Promise.reject(new Error("denied")) },
  });
  Object.defineProperty(document, "execCommand", { configurable: true, value: () => false });
  return () => {
    Object.defineProperty(document, "execCommand", { configurable: true, value: execCommand });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
  };
}
