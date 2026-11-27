import { createContext } from "react";

/**
 * The titles of the issues a page already holds, by key, which a reference to one of them draws
 * until that issue's own summary answers (`useReferenceTarget`). A reference standing on its own
 * reads as the issue's key and title once its summary is read (`RefLink`); drawn first as the
 * words the author wrote, it grew as the summary answered, a line taller where the title wrapped,
 * and moved every row under it (R3; the clock rows' Row 2 at 1440 px, the rows of comments that
 * name a child issue). The new issue page holds its issue's children, with their titles, before it
 * draws a row (`IssueOutlinePage`), and gives them here, so such a reference is drawn with the
 * words its summary will draw from the first drawing. No other page gives any: there every
 * reference reads as it did.
 */
export const HeldIssueTitles = createContext<ReadonlyMap<string, string> | undefined>(undefined);
