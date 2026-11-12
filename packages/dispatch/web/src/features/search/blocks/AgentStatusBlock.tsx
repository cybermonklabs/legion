import type { ReactNode } from "react";

import { type BlockOf, cellText } from "./block-cells";
import { blockCellClassName, blockHeadClassName } from "./TableBlock";

/** The agents' status: each session the roster read returned, with its state, its open asks and
 *  when it was last active, as Dispatch's copy of the roster holds them. */
export function AgentStatusBlock({
  block,
}: {
  readonly block: BlockOf<"agent_status">;
}): ReactNode {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs" data-testid="going-block-agents">
        <thead>
          <tr>
            <th className={blockHeadClassName} scope="col">
              Agent
            </th>
            <th className={blockHeadClassName} scope="col">
              State
            </th>
            <th className={blockHeadClassName} scope="col">
              Open asks
            </th>
            <th className={blockHeadClassName} scope="col">
              Last active
            </th>
          </tr>
        </thead>
        <tbody>
          {block.rows.map((row) => {
            const session = cellText(row.session_id);
            return (
              <tr data-session={session} data-testid="going-block-agent" key={session}>
                <td className={blockCellClassName}>{cellText(row.title) || session}</td>
                <td className={blockCellClassName}>{cellText(row.state)}</td>
                <td className={blockCellClassName} data-testid="going-block-agent-asks">
                  {cellText(row.open_asks)}
                </td>
                <td className={blockCellClassName}>{cellText(row.last_activity)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
