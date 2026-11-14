import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { isLegionNotConfigured } from "../../api/client";
import { legionBoardQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { textMutedOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { LegionSettingsPanel } from "../legion/LegionSettingsPanel";

/** `/projects/<KEY>/settings`: a project tab shown for a project with a Legion daemon, holding
 *  the daemon's settings panel. The panel reads the daemon's settings off the board route and
 *  its live push, as it did inside the Legion view. */
export function ProjectSettings({ project }: { readonly project: string }): ReactNode {
  const board = useQuery(legionBoardQuery({ kind: "legion", project }));
  return (
    <section aria-label="Project settings" className="min-w-0" data-project-settings={project}>
      {board.isPending ? <p className={textMutedOnCanvas}>Loading settings…</p> : null}
      {isLegionNotConfigured(board.error) ? (
        <p className={`text-sm ${textSecondaryOnCanvas}`}>
          {project} no longer names a Legion daemon
        </p>
      ) : board.isError ? (
        <QueryError
          message="Could not load the daemon's settings."
          onRetry={() => void board.refetch()}
          retrying={board.isFetching}
        />
      ) : null}
      {board.data === undefined ? null : <LegionSettingsPanel board={board.data} />}
    </section>
  );
}
