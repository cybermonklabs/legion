import type { VisibilityState } from "@tanstack/react-table";
import { useMemo } from "react";
import { functionalUpdate } from "../../components/DataTable";
import {
  type LayoutColumns,
  type TableLayoutModel,
  useTableLayout,
} from "../../components/table-layout";
import { type TableAddress, useAddressUpdate } from "../../lib/page-address";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { ROW_ACTIONS_WIDTH } from "./issues-table-columns";
import {
  fitViewWidths,
  isBoardFedColumn,
  tableStorageKey,
  type View,
  viewLayoutColumns,
} from "./issues-table-model";
import {
  type BuiltInView,
  readCols,
  shownColumns,
  viewColumns,
  viewLayoutKey,
} from "./issues-views";

/** The columns the Issues tab draws first on a phone: Select and Key, which it pins, and Title,
 *  which scrolls with the table and fills the rest of the row (R10, `viewLayoutColumns`). */
const PHONE_LEAD: readonly string[] = ["select", "key", "title"];

/** `ids` as a phone draws them: `PHONE_LEAD` first, then every other column in its order. */
const phoneOrder = (ids: readonly string[]): string[] => [
  ...PHONE_LEAD,
  ...ids.filter((id) => !PHONE_LEAD.includes(id)),
];

/** The one column of `PHONE_LEAD` a phone does not pin, which no drag moves from after Key. */
const PHONE_FIXED: readonly string[] = ["title"];

/**
 * The layout of the table of one view of the Issues tab of `project`, by the view's columns:
 * the address's `cols=` for this load, else the view's own (`base`), with no board-fed column in
 * a project with no daemon (`hasLegion`). Its widths and order are kept under the view's key
 * (`viewLayoutKey`); its column visibility too, except while `cols=` is in the address, which
 * holds for that load and writes nothing: a change of columns then writes `cols=`. Until the
 * reader drags a width, the columns are fitted to the box (`fitViewWidths`); on a phone Title
 * follows Select and Key, whatever the reader drags (`phoneOrder`). The stored layout is read once
 * as the table mounts, so its caller draws one table per view's key.
 */
export function useTabTableLayout(
  address: TableAddress<View>,
  { base, hasLegion, project }: { base: BuiltInView; hasLegion: boolean; project: string }
): { readonly columnsLayout: LayoutColumns; readonly layout: TableLayoutModel } {
  const update = useAddressUpdate();
  const { params } = address;
  // Below 1280 px every button is a 44 px touch target, and each column's minimum holds its
  // header with that filter button, and its sort mark too while the view is sorted by it.
  const touch = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  // `cols=` for this load, read again only when the address moves.
  const cols = useMemo(() => readCols(params), [params]);
  const rules = viewLayoutColumns(shownColumns(params, base, hasLegion), touch);
  // What the stored layout keeps hidden until the reader shows it, whatever the address and the
  // daemon setting say now: the view's own columns in a project with a daemon. So a load with
  // `cols=`, or one drawn with no daemon while the setting's read is out, writes back none of the
  // columns it hid for that load alone.
  const ownRules = viewLayoutColumns(viewColumns(base, true), touch);
  const storedRules = useMemo<LayoutColumns>(
    () => ({ ...rules, hiddenByDefault: ownRules.hiddenByDefault }),
    [rules, ownRules]
  );
  const stored = useTableLayout(
    viewLayoutKey(tableStorageKey(project), base.id),
    storedRules,
    address.view.sorting
  );
  // No board-fed column shows in a project with no daemon, whatever a reader stored while one
  // ran, and Queue only once the reader shows it. Held while what it reads stays, so the table's
  // cells are not drawn again for a new object of the same visibility.
  const visibility = useMemo<VisibilityState>(() => {
    const shown = cols === undefined ? undefined : new Set(["select", ...cols]);
    const unfed: VisibilityState = hasLegion
      ? {}
      : {
          queue: shown?.has("queue") ?? stored.visibility.queue ?? false,
          ...Object.fromEntries(rules.ids.filter(isBoardFedColumn).map((id) => [id, false])),
        };
    return shown === undefined
      ? { ...stored.visibility, ...unfed }
      : { ...Object.fromEntries(rules.ids.map((id) => [id, shown.has(id)])), ...unfed };
  }, [cols, hasLegion, rules.ids, stored.visibility]);
  const drawn = stored.order.length > 0 ? stored.order : rules.ids;
  const phone = stored.boxWidth < 640;
  const order = phone ? phoneOrder(drawn) : stored.order;
  // A header dropped on a phone keeps Select, Key and Title first, and what is stored is the
  // order drawn, so a reload draws it again (R10). The core puts a dropped column before the
  // header it lands on: one dropped onto Title lands right after it once Title is put back after
  // Key, and one dropped onto another header moves among the columns after Title. Title itself
  // is not dragged there (`fixedColumns`).
  const setPhoneOrder: TableLayoutModel["setOrder"] = (next) =>
    stored.setOrder(phoneOrder(functionalUpdate(next, order)));
  const writeCols: TableLayoutModel["setVisibility"] = (next) => {
    const resolved = functionalUpdate(next, visibility);
    const ids = rules.ids.filter((id) => id !== "select" && resolved[id] !== false);
    update((current) => {
      current.set("cols", ids.join(","));
      return current;
    });
  };
  // The reader's own widths, or a phone's, are drawn as they are; else the columns are fitted to
  // the box, again only when what the fit reads changes.
  const ownWidths = Object.keys(stored.sizing).length > 0 || phone;
  const responsive = useMemo(
    () =>
      ownWidths
        ? stored.responsive
        : {
            ...stored.responsive,
            sizing: fitViewWidths(
              drawn.filter((id) => visibility[id] !== false),
              stored.responsive.sizing,
              stored.minimums,
              stored.boxWidth,
              ROW_ACTIONS_WIDTH
            ),
          },
    [ownWidths, drawn, visibility, stored.responsive, stored.minimums, stored.boxWidth]
  );
  return {
    columnsLayout: rules,
    layout: {
      ...stored,
      order,
      setOrder: phone ? setPhoneOrder : stored.setOrder,
      fixedColumns: phone ? PHONE_FIXED : undefined,
      responsive,
      visibility,
      ...(cols === undefined ? {} : { setVisibility: writeCols }),
    },
  };
}
