import { appendFileSync } from "node:fs";

/**
 * Tests only: the record a seam of the program writes, one JSON line for each step, to the file
 * `env[variable]` names, so a test on the other side of a process boundary reads what the program
 * chose (a bound it armed, what ended a wait) from the program's own record rather than from a
 * clock. Undefined where the variable names no file, as every deployment runs. A write that fails
 * is dropped: the record never changes what the program does.
 */
export function jsonLineRecord<T>(
  env: Record<string, string | undefined>,
  variable: string
): ((step: T) => void) | undefined {
  const file = env[variable];
  if (file === undefined || file === "") return undefined;
  return (step) => {
    try {
      appendFileSync(file, `${JSON.stringify(step)}\n`);
    } catch {
      // The tests' seam only: a write it cannot make changes nothing the program does.
    }
  };
}
