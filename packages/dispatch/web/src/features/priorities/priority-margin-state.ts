import { useUserPreference } from "../shell/userPreference";

/** The four states of a margin, which are the four filters over one list and not four lists:
 *  a tab and a state are the same object, so a reader learns one (dispatch://INFRA-1196). */
export const MARGIN_STATES = [
  { id: "words", label: "Words anyone wrote" },
  { id: "questions", label: "Questions" },
  { id: "pinned", label: "Pinned" },
  { id: "everything", label: "Everything" },
] as const;

export type MarginState = (typeof MARGIN_STATES)[number]["id"];

const DEFAULT_STATE: MarginState = "everything";

function isMarginState(value: unknown): value is MarginState {
  return MARGIN_STATES.some((state) => state.id === value);
}

/** The state the reader last chose, which is the one the margin opens with when they come
 *  back. It is that person's own, kept under their login like every other browser preference,
 *  so a shared browser never opens one person's margin on the next person's choice. A browser
 *  that refuses storage opens on Everything, as a first visit does. */
export function useMarginState(): [MarginState, (next: MarginState) => void] {
  return useUserPreference(
    "priorities.margin",
    (stored) => (isMarginState(stored) ? stored : DEFAULT_STATE),
    (state) => state
  );
}
