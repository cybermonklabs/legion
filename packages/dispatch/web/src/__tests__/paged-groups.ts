import { spyOn } from "bun:test";

import { api } from "../api/client";
import type { AccessGroup } from "../api/types";

/** `GET /api/v1/access-groups` answering `pages` as Dispatch pages the list: each page but the last
 *  names, as its `next`, the last group on it, which the next read passes as `after`. It spies the
 *  one page read, so `api.listAccessGroups` itself reads them, as the page does. */
export function pagedGroups(pages: AccessGroup[][]) {
  return spyOn(api, "listAccessGroupsPage").mockImplementation(async (after) => {
    const index =
      after === undefined ? 0 : pages.findIndex((page) => page.at(-1)?.name === after) + 1;
    const groups = pages[index] ?? [];
    const next = groups.at(-1)?.name;
    return index < pages.length - 1 && next !== undefined ? { groups, next } : { groups };
  });
}
