import type { ReactNode } from "react";

import {
  TableBody,
  TableCell,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "../../../components/TableShell";
import { type BlockOf, cellText } from "./block-cells";

/** The agents' status: each session the roster read returned, with its state, its open asks and
 *  when it was last active, as Dispatch's copy of the roster holds them, in the shared table
 *  (`TableShell`). */
export function AgentStatusBlock({
  block,
}: {
  readonly block: BlockOf<"agent_status">;
}): ReactNode {
  return (
    <div data-testid="going-block-agents">
      <TableShell label="Agents" size="compact">
        <TableHead>
          <TableHeadCell>Agent</TableHeadCell>
          <TableHeadCell>State</TableHeadCell>
          <TableHeadCell>Open asks</TableHeadCell>
          <TableHeadCell>Last active</TableHeadCell>
        </TableHead>
        <TableBody>
          {block.rows.map((row) => {
            const session = cellText(row.session_id);
            return (
              <TableRow data-session={session} data-testid="going-block-agent" key={session}>
                <TableCell>{cellText(row.title) || session}</TableCell>
                <TableCell>{cellText(row.state)}</TableCell>
                <TableCell data-testid="going-block-agent-asks">
                  {cellText(row.open_asks)}
                </TableCell>
                <TableCell>{cellText(row.last_activity)}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </TableShell>
    </div>
  );
}
