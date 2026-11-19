/**
 * The search for every line that prints a setting's value that can be a URL: a variable of the
 * environment, an argument of the command line or a field of a file that the program reads, which
 * can hold a user and a password.
 *
 *   bun scripts/checks/setting-prints.ts [--all] [--base <rev> | --scope <file>]
 *
 * It follows the DATA, not names. A value is followed from where the program reads it
 * (`process.env`, `Bun.env`, `import.meta.env`, `process.argv`, `Bun.argv`, and what a file read
 * gives) through every name, field, parameter, return value, array, object, template, `+`, `join`
 * and parse error it reaches, whatever those are called, to every line that prints it, logs it or
 * writes it into a command: a `console` call, a write to standard output or standard error, a log
 * call, the message of an error, a `throw`, a child's command line, and the error of a `new URL(…)`
 * that Bun prints whole. The naming functions of a configured URL, `urlForText` and
 * `requestForText` (and `unshownSettingUrl`, which names a value by its length), end a value's
 * path, and so does a value whose type cannot hold text (a number, a boolean, a set of literal
 * strings). `shownSettingUrl` ends none: it decides whether a value is admitted, and what it
 * answers can still hold a piece of a password, so a line that prints its answer prints the
 * setting. A `new URL(…)` of a setting writes no line only where each value it is handed, the base
 * too, is one a guard admitted, whole: an `if` before it, in a block around it, that ends the
 * function, the loop or the program when `shownSettingUrl` does not admit that same value (or
 * `URL.canParse` refuses it), or a branch (`if`, `?:`, `&&`, `||`) taken only when the judge
 * admits it. The value is a `const`, a parameter its function never assigns, or a member of one
 * that no line of the file assigns, or a choice (`?:`, `??`) of such values; the guard stands in
 * the same function, or in one that makes the arrow or function expression the parse is in after
 * the guard. A judge whose answer nothing reads, or one that runs after the parse, admits nothing,
 * and neither does a guard of a value that the parse is handed built into another (a template, a
 * `+`) or with an input against it as its base: Bun prints the base in the error of a parse whose
 * input fails.
 *
 * A line it reports is where a setting AS READ is written into text: the template, `+`, `join`,
 * `String` or format that writes it, a parse error that holds it, or a print of the raw value. A
 * print of text that another line built is an end of that line, not a second line. A function's
 * answer is read for each call from what that call hands it: the line that writes a parameter into
 * the answer is that call's line, with that call's settings only, and a setting read inside the
 * function is named for each call that answers with it.
 *
 * Which settings can be a URL is read from the data too: a variable of the environment (by its
 * name, wherever it is read), an argument or a field of a file is one when its value is the URL a
 * URL taker is handed, or the head of its text (`new URL`, `fetch`, a request, a socket, a NATS or
 * Postgres client, ky, `shownSettingUrl` and the naming functions), or when it is handed to another
 * program (a child's command line or environment), whose use of it the search cannot read. A read
 * of the environment by a key the search cannot name is always one.
 *
 * The Go half, `scripts/checks/setting-prints/go/`, does the same over the Go Dispatch server's
 * command and routes packages, where the one function is `redact.URL`; this program runs it and
 * reads its lines.
 *
 * This file is the command. Its parts are in `scripts/checks/setting-prints/`, one concern each:
 * loading what the search reads (`program.ts`, and `go.ts` for the Go half's records), the planted
 * shapes that are its controls (`planted.ts`), the analysis (`analysis.ts`, over its model
 * `model.ts`, its rules of the language and the libraries `rules.ts`, its labels `labels.ts` and
 * its reading of guards `guards.ts`), and what it prints (`report.ts`).
 *
 * Its scope is the files this pull request changes (what differs from where the branch left
 * `--base`, origin/main unless named, or the list `--scope` names), and each file whose written
 * text a line of those files prints. Each line it finds there is either fixed, so that it names
 * the value through `urlForText` or `requestForText`, or listed in `setting-prints.judged.ts` with
 * why it cannot carry a user part. A line's text is its own, trimmed; a line that holds no word (a
 * bare `{`) runs on to the first line after it that does, so its judgement names it apart from the
 * file's other lines. It exits 0 when every line it finds is judged, and 1 when one is not or when
 * a judgement names a line it no longer finds. Each run also reads planted shapes (`PLANTED`, and
 * the Go half's own): each line marked `found` there must be found and each marked `quiet` must
 * not, else it says UNKNOWN and exits 2, since a search that cannot see those shapes would say the
 * same about the tree whether it looked or not. A line of the tree that only planted lines print
 * is the planted file's own and counts in no verdict of the tree: a plant that prints what
 * `shownSettingUrl` answers makes the line of `setting-url.ts` that writes it a finding, which a
 * judgement would then hide for every other line that prints it. `--all` prints the judged lines
 * and the lines of settings that reach no URL too.
 *
 * What it reads: every TypeScript file git tracks, but tests (`*.test.ts`, `*.spec.ts`, `*.e2e.ts`,
 * and `__tests__`, `test`, `tests` and `fixtures` directories) and declarations. What it does not
 * follow: a value through a library function other than the string, array, object, path, URL, JSON
 * and parse functions it names (`KEEPS`), so a library's own error is not read; an error a `throw`
 * hands to a catch (the `throw` is an end itself); a call through an interface whose implementation
 * it cannot resolve; a value that leaves through a network answer or a file; JSX. A field of an
 * object type the tree declares is read from every write of it, in every function. A pass is a
 * claim about those shapes and no other.
 */
import path from "node:path";
import { search } from "./setting-prints/analysis";
import { staleBases } from "./setting-prints/bases";
import { goFindings } from "./setting-prints/go";
import { PLANTED, plantedBlind, plantedFile, plantedOwn } from "./setting-prints/planted";
import { changedFiles, inScope, readPath, root } from "./setting-prints/program";
import { printFinding, printShaken, printStale, verdict } from "./setting-prints/report";
import { JUDGED, type Judged } from "./setting-prints.judged";

if (import.meta.main) {
  const all = process.argv.includes("--all");
  const scope = changedFiles();
  if (scope === undefined || scope.size === 0) {
    console.log(
      "UNKNOWN: git names no file this pull request changes; name them with --scope <file>"
    );
    process.exit(2);
  }
  const listed = Bun.spawnSync(["git", "ls-files", "-z"], { cwd: root, stdout: "pipe" })
    .stdout.toString()
    .split("\0");
  const files = listed
    .filter((file) => file !== "" && readPath(file))
    .map((file) => path.join(root, file));
  const typescript = search(files, new Map([[plantedFile, PLANTED]]));
  const go = goFindings();
  const blind = [...plantedBlind(typescript.findings), ...go.blind];
  const everywhere = [...typescript.findings.filter((f) => !plantedOwn(f)), ...go.findings];
  const findings = everywhere.filter((f) => inScope(f, scope));
  const used = new Set<Judged>();
  let open = 0;
  for (const finding of findings) {
    const judged = JUDGED.find((j) => j.file === finding.file && finding.text.includes(j.text));
    if (judged !== undefined) used.add(judged);
    else open += 1;
    if (judged === undefined || all) printFinding(finding, judged, false);
  }
  const quiet = [...typescript.quiet.filter((f) => !plantedOwn(f)), ...go.quiet].filter((f) =>
    inScope(f, scope)
  );
  if (all) for (const finding of quiet) printFinding(finding, undefined, true);
  const stale = JUDGED.filter((j) => !used.has(j));
  for (const j of stale) printStale(j);
  // A judgement that names its basis stands only while that basis is as it was judged.
  const shaken = [...used].flatMap((j) => staleBases(j).map((why) => ({ judged: j, why })));
  for (const { judged, why } of shaken) printShaken(judged, why);
  const status = verdict({
    scope: scope.size,
    outside: everywhere.length - findings.length,
    files: typescript.files,
    printing: typescript.printing,
    rounds: typescript.rounds,
    quiet: quiet.length,
    go: go.summary,
    blind,
    found: findings.length,
    open,
    stale: stale.length,
    shaken: shaken.length,
  });
  if (status !== 0) process.exit(status);
}
