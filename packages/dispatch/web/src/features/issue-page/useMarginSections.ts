import { userPreferenceStorageKey, useUserPreference } from "../shell/userPreference";
import type { MarginSection } from "./page-structure";

/**
 * Which of Details' property sections the reader has closed, kept for the next issue they open,
 * the way the margin's own width and the pins are kept: a browser preference of the signed-in
 * identity (`features/shell/userPreference.ts`).
 *
 * Every section opens expanded, so a first load shows every property it holds; what is stored is
 * what the reader closed, which means a section added later opens expanded too.
 */
export interface MarginSections {
  readonly isOpen: (section: MarginSection) => boolean;
  readonly toggle: (section: MarginSection) => void;
}

/** The preference the sections are kept under. */
const SECTIONS_PREFERENCE = "issue.margin-sections";

/** The sections the reader closed, from what their browser stores: none where it stores nothing. */
function readClosed(stored: string | null): readonly string[] {
  return stored === null ? [] : stored.split(",").filter((name) => name !== "");
}

export function useMarginSections(): MarginSections {
  const [collapsed, setCollapsed] = useUserPreference<readonly string[]>(
    SECTIONS_PREFERENCE,
    readClosed,
    (sections) => sections.join(",")
  );
  return {
    isOpen: (section) => !collapsed.includes(section),
    toggle: (section) =>
      setCollapsed(
        collapsed.includes(section)
          ? collapsed.filter((name) => name !== section)
          : [...collapsed, section]
      ),
  };
}

/** Whether `section` is open for the reader signed in as `login`, read from what their browser
 *  stores, as `useMarginSections` reads it as it mounts: what a press's handler reads, which no
 *  hook's state reaches (`IssueMargin.tsx`'s `startDetailsReads`). With no login known, every
 *  section is open, as the hook draws it then. */
export function marginSectionOpen(login: string | undefined, section: MarginSection): boolean {
  const stored =
    login === undefined
      ? null
      : window.localStorage.getItem(userPreferenceStorageKey(login, SECTIONS_PREFERENCE));
  return !readClosed(stored).includes(section);
}
