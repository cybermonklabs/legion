// scripts/e2e/lib/psql-statement.ts: one statement through psql on the database a connection URI
// names, with the URI's password kept out of every argument list. psql takes the URI without its
// password as its argument and the password from PGPASSWORD, set in that one command's
// environment alone, so no process listing (`ps -ww`, /proc/<pid>/cmdline) shows it while psql
// runs. scripts/e2e/verification-results.ts runs its statements through it.

/** psql's command and environment for `args` on the database `uri` names. */
export function psqlCommand(
  uri: string,
  args: readonly string[],
  env: Record<string, string | undefined> = process.env
): { readonly cmd: string[]; readonly env: Record<string, string | undefined> } {
  const url = new URL(uri);
  const password = decodeURIComponent(url.password);
  url.password = "";
  return {
    cmd: ["psql", url.toString(), ...args],
    env: password === "" ? env : { ...env, PGPASSWORD: password },
  };
}

/** Runs `statement` against the database `uri` names and answers what psql printed, rows bare and
 *  columns separated by `|` (`-tA`); a psql that exits non-zero is an error carrying its stderr. */
export async function psqlStatement(
  uri: string,
  statement: string,
  env: Record<string, string | undefined> = process.env
): Promise<string> {
  const command = psqlCommand(
    uri,
    ["-X", "-q", "-v", "ON_ERROR_STOP=1", "-tA", "-c", statement],
    env
  );
  const child = Bun.spawn(command.cmd, { env: command.env, stderr: "pipe", stdout: "pipe" });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (code !== 0) throw new Error(`psql exited ${code}: ${stderr.trim()}`);
  return stdout.trim();
}
