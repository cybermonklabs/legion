/**
 * The box of the shared table (`components/TableShell.tsx`), as a page's case finds it: by the
 * name the component draws on it (`CONTROL.tableShell`) rather than a copy of that name, so a
 * case that reads the box - whether it takes a Tab stop of its own - reads the one the page draws.
 */

import { CONTROL } from "../components/control-names";

/** The selector of the shared table's box (`TableShell`). */
export const TABLE_SHELL = `[data-control="${CONTROL.tableShell}"]`;
