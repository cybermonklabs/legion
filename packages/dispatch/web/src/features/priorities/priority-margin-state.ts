/** The four states of a margin, which are the four filters over one list and not four lists:
 *  a tab and a state are the same object, so a reader learns one (dispatch://INFRA-1196). */
export const MARGIN_STATES = [
  { id: "words", label: "Words anyone wrote" },
  { id: "questions", label: "Questions" },
  { id: "pinned", label: "Pinned" },
  { id: "everything", label: "Everything" },
] as const;

export type MarginState = (typeof MARGIN_STATES)[number]["id"];

const STORAGE_KEY = "dispatch.priority-margin";
const DEFAULT_STATE: MarginState = "everything";

function isMarginState(value: unknown): value is MarginState {
  return MARGIN_STATES.some((state) => state.id === value);
}

/** The state the reader last chose, which is the one the margin opens with when they come
 *  back. A browser that refuses storage opens on Everything, as a first visit does. */
export function readMarginState(): MarginState {
  try {
    const stored: unknown = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "null");
    return isMarginState(stored) ? stored : DEFAULT_STATE;
  } catch {
    return DEFAULT_STATE;
  }
}

export function writeMarginState(state: MarginState): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
  } catch {
    /* The margin still works when the browser refuses local preferences. */
  }
}
