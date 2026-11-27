/**
 * The words of a table whose rows are deploys, not issues, which the shared table's unit rows
 * draw (`DataTable.test.tsx`, `ColumnFilter.test.tsx`, `Pager.test.tsx`): the core and its
 * controls know nothing of what their rows are, so each reads its words from its caller.
 */
import type { DataTableNames, RowNoun } from "../components/DataTable";

/** The caller's own words for its deploys, which the core reads: the scope its keys are
 *  registered in, the actions of the page that draws it, and the attributes its rows and links
 *  carry. */
export const DEPLOY_NAMES: DataTableNames = {
  groupAttribute: "data-deploy-group",
  keymap: {
    next: "project.next",
    open: "project.open",
    openOnEnter: "project.open-enter",
    previous: "project.previous",
    scope: "agents",
    select: "project.select",
  },
  linkAttribute: "data-deploy-link",
  rowAttribute: "data-deploy-row",
};

/** What the deploys table calls its rows, which its filters and its pager count in. */
export const DEPLOY_NOUN: RowNoun = {
  group: { one: "group", many: "groups" },
  line: { one: "row", many: "rows" },
  many: "deploys",
  one: "deploy",
};
