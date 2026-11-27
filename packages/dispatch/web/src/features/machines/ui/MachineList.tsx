import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { textMutedOnCanvas } from "../../../theme/classes";
import { borderDefault, card, textPrimaryOnCanvas } from "../../../theme/shell-classes";
import { buildMachinePath } from "../../refs/routes";
import { type MachineSummary, sessionCountSentence } from "../machine-model";

/** The machines a live session names, one row each, each a link to its machine's page. */
export function MachineList({ machines }: { machines: readonly MachineSummary[] }): ReactNode {
  return (
    <ul className="space-y-2" data-testid="machine-list">
      {machines.map((machine) => (
        <li
          className={`rounded-xl border ${card} ${borderDefault}`}
          data-in-tmux={machine.inTmux}
          data-machine={machine.host}
          data-sessions={machine.sessions}
          data-testid="machine-row"
          key={machine.host}
        >
          <Link className="flex w-full flex-col px-3 py-2" to={buildMachinePath(machine.host)}>
            <span className={`min-w-0 break-words font-medium ${textPrimaryOnCanvas}`}>
              {machine.host}
            </span>
            <span className={`text-xs ${textMutedOnCanvas}`}>{sessionCountSentence(machine)}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}
