// Oh My Pi v18.16.0, packages/utils/src/dirs.ts:43-44,84-96,334-337,997-1000.
// A subagent's path is below its parent's, so this also reads its root's profile.
const NAME = "[a-z0-9][a-z0-9._-]{0,63}";
export const DEFAULT_ROOT = "^(?:/|~/)(?:[^/]+/)*?[.]omp/agent/sessions/.+";
export const NAMED_ROOT = `^(?:/|~/)(?:[^/]+/)*?[.]omp/profiles/(${NAME})/agent/sessions/.+`;
const named = new RegExp(NAMED_ROOT);
const implicit = new RegExp(DEFAULT_ROOT);

/** The same classification that roots.sql emits, with no home or transcript path. */
export function sessionProfileName(transcriptPath: string | undefined): string {
  if (transcriptPath === undefined) return "unknown";
  return named.exec(transcriptPath)?.[1] ?? (implicit.test(transcriptPath) ? "default" : "unknown");
}
