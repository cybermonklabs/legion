import { EVENT_FRESH } from "../refs/reference-target";
import { useUserPreference } from "../shell/userPreference";
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

export function useMarginSections(): MarginSections {
  const [collapsed, setCollapsed] = useUserPreference<readonly string[]>(
    "issue.margin-sections",
    (stored) => (stored === null ? [] : stored.split(",").filter((name) => name !== "")),
    (sections) => sections.join(","),
    // Who is reading, kept fresh as the page's other readers keep it (`useWhoIsReading`).
    { freshness: EVENT_FRESH }
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
