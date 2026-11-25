/** Records each Escape pressed while it listens, from `document`'s capture phase, so a case can
 *  read once the press is over whether a handler marked it `defaultPrevented`. */
export function recordEscapes(): { readonly presses: KeyboardEvent[]; readonly stop: () => void } {
  const presses: KeyboardEvent[] = [];
  const record = (event: KeyboardEvent) => {
    if (event.key === "Escape") presses.push(event);
  };
  document.addEventListener("keydown", record, true);
  return { presses, stop: () => document.removeEventListener("keydown", record, true) };
}
