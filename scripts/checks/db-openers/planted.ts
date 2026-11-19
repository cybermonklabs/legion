/**
 * The planted shapes of the TypeScript half: one more file the search reads with the tree's own. A
 * line marked `planted: <verdict>` must be listed with that verdict, and a line marked neither must
 * not be listed. The modules it imports need not be installed: the search reads syntax only.
 */
export const plantedName = "scripts/checks/db-openers.planted.ts";

export const PLANTED = `import { SQL, sql, type TransactionSQL } from "bun";
import * as bun from "bun";
import postgres from "postgres";
import pg, { Pool } from "pg";

const LOCAL = "postgres://127.0.0.1:5432/planted";

export async function planted(url: string, tx: TransactionSQL): Promise<void> {
  new SQL(process.env.DATABASE_URL ?? ""); // planted: unjudged
  new SQL(url); // planted: unjudged
  new SQL(LOCAL); // planted: judged
  new SQL("postgres://127.0.0.1:5432/planted"); // planted: judged
  new SQL(); // planted: unjudged
  new bun.SQL(url); // planted: unjudged
  new Bun.SQL(url); // planted: unjudged
  const Aliased = SQL;
  new Aliased(url); // planted: unjudged
  const { SQL: Destructured } = Bun;
  new Destructured(url); // planted: unjudged
  await sql\`select 1\`; // planted: unjudged
  await sql.begin(async () => undefined); // planted: unjudged
  postgres(url); // planted: unjudged
  new Pool({ connectionString: url }); // planted: unjudged
  new pg.Client({ connectionString: LOCAL }); // planted: judged
  const openers = [SQL]; // planted: unjudged
  const typed: SQL | undefined = undefined;
  void openers;
  void typed;
  void tx;
  await import("pg"); // planted: unjudged
}
`;

/** What the planted shapes gave that was not as marked, given the verdict of each planted line
 *  the search listed. */
export function plantedBlind(listed: ReadonlyMap<number, string>): string[] {
  const blind: string[] = [];
  PLANTED.split("\n").forEach((text, index) => {
    const line = index + 1;
    const mark = /\/\/ planted: (judged|unjudged)$/.exec(text)?.[1];
    const got = listed.get(line);
    if (mark !== undefined && got === undefined)
      blind.push(`the planted line ${plantedName}:${line} was not listed`);
    else if (mark !== undefined && got !== mark)
      blind.push(`the planted line ${plantedName}:${line} was listed ${got}, not ${mark}`);
    else if (mark === undefined && got !== undefined)
      blind.push(`the planted line ${plantedName}:${line}, which is not marked, was listed ${got}`);
  });
  return blind;
}
