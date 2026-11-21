import { type ReactNode, useState } from "react";

import type { Project } from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import { dangerText } from "../../theme/classes";
import { settingsFieldLabel, settingsListText, settingsListTrigger } from "./classes";

/** The "Choose a project" field both Settings forms use: its visible `label` over the shared
 *  list's single pick (`MultiSelect`) of the projects, `KEY · name`, whose text box narrows them
 *  as the reader types, and whose trigger's name starts with `label`. Its host draws it once the
 *  projects are read, so the list never waits on them. A button holds no `required`: its host
 *  files nothing while `value` is empty, and says so with `NoProjectPicked`. `held` while its
 *  form's write is out: the trigger keeps the focus and its Tab stop (`aria-disabled`), and a
 *  pick made then is dropped. */
export function ProjectSelect({
  held = false,
  label,
  onChange,
  projects,
  value,
}: {
  held?: boolean;
  label: string;
  onChange: (project: string) => void;
  projects: readonly Project[];
  value: string;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const labels = new Map(
    projects.map((candidate) => [candidate.key, `${candidate.key} · ${candidate.name}`])
  );
  return (
    <div className={settingsFieldLabel}>
      {/* The list is named for the field itself, so its words here are for the eye alone. */}
      <span aria-hidden="true">{label}</span>
      <MultiSelect
        ariaDisabled={held}
        emptyMessage={projects.length > 0 ? "No project matches that." : "No projects yet"}
        label={label}
        mode="single"
        onOpenChange={setOpen}
        onPick={(picked) => {
          if (!held) onChange(picked);
        }}
        open={open}
        optionLabel={(key) => labels.get(key)}
        options={projects.map((candidate) => candidate.key)}
        searchLabel="Search projects"
        triggerClassName={settingsListTrigger}
        triggerTextClassName={settingsListText}
        value={value === "" ? undefined : value}
      >
        {value === "" ? "Choose a project" : undefined}
      </MultiSelect>
    </div>
  );
}

/** What a form holding a `ProjectSelect` says, under itself, when it was submitted with no
 *  project picked and filed nothing: the browser said as much of the `required` select the list
 *  replaced. */
export function NoProjectPicked(): ReactNode {
  return (
    <p className={`mt-4 text-sm ${dangerText}`} role="alert">
      Choose a project.
    </p>
  );
}
