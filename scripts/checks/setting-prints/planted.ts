/**
 * The planted shapes, the search's controls: one more file read with the tree, whose lines marked
 * `found` must be found and whose lines marked `quiet` must not be, else the search cannot see a
 * shape it must see.
 */
import path from "node:path";
import type { Finding } from "./model";
import { root } from "./program";

/** The planted shapes. A line that ends `// planted: found` must be found, and one that ends
 *  `// planted: quiet` must not be; a line that is neither must not be found either, since it
 *  writes no setting as read. They are read with the tree, as one more file of it. */
export const PLANTED = `import { requestForText, urlForText } from "${root}/packages/contracts/src/log-text";
import { shownSettingUrl } from "${root}/packages/contracts/src/setting-url";

interface PlantedBanner {
  readonly env: Readonly<Record<string, string | undefined>>;
}
function plantedQuote(value: string): string {
  return /^[a-z]+$/.test(value) ? value : \`'\${value}'\`; // planted: found
}
function plantedCommand(banner: PlantedBanner): string {
  const words = ["PLANTED_URL"].map((name) => \`\${name}=\${plantedQuote(banner.env[name] ?? "")}\`); // planted: found
  return ["PLANTED=1", ...words].join(" ");
}
console.log("run: " + plantedCommand({ env: process.env }));
console.log(plantedQuote("a constant")); // planted: quiet
const plantedArgument = process.argv[2] ?? "";
async function plantedFetch(where: string): Promise<void> {
  await fetch(new URL("/planted", where)); // planted: found
}
await plantedFetch(plantedArgument);
await fetch(process.env.PLANTED_ERROR ?? "");
function plantedError(): never {
  throw new Error(\`planted \${process.env.PLANTED_ERROR}\`); // planted: found
}
plantedError();
const plantedWords = ["planted", Bun.env.PLANTED_JOINED];
process.stderr.write(\`\${plantedWords.join(" ")}\\n\`); // planted: found
console.warn("planted: " + Bun.env.PLANTED_JOINED); // planted: found
try {
  new URL(Bun.env.PLANTED_JOINED ?? ""); // planted: found
} catch (error) {
  console.error(String(error));
}
console.log(\`a home that is never a URL: \${process.env.PLANTED_HOME}\`); // planted: quiet
console.log(\`admitted \${shownSettingUrl(process.env.PLANTED_JUDGED ?? "") ?? "nothing"}\`); // planted: found
console.log(\`named \${urlForText(process.env.PLANTED_NAMED ?? "", "PLANTED_NAMED")}\`); // planted: quiet
const plantedRequested = process.env.PLANTED_NAMED ?? "";
console.log(\`asked \${requestForText(plantedRequested, \`\${plantedRequested}/planted\`, "PLANTED_NAMED")}\`); // planted: quiet
console.log(\`a port \${Number(process.env.PLANTED_ERROR)}\`); // planted: quiet
const plantedAdmitted = process.env.PLANTED_ADMITTED ?? "";
if (shownSettingUrl(plantedAdmitted) === undefined) process.exit(2);
const plantedOrigin = new URL(plantedAdmitted).origin; // planted: quiet
void plantedOrigin;
const plantedBuilt = new URL(\`\${plantedAdmitted}:planted\`).host; // planted: found
void plantedBuilt;
const plantedBased = new URL("/planted", plantedAdmitted).host; // planted: found
void plantedBased;
const plantedIgnored = process.env.PLANTED_IGNORED ?? "";
shownSettingUrl(plantedIgnored);
const plantedIgnoredHost = new URL(plantedIgnored).host; // planted: found
void plantedIgnoredHost;
const plantedLate = process.env.PLANTED_LATE ?? "";
const plantedLateHost = new URL(plantedLate).host; // planted: found
if (shownSettingUrl(plantedLate) === undefined) process.exit(2);
void plantedLateHost;
`;
export const plantedFile = path.join(root, "scripts/checks/setting-prints.planted.ts");

/** The planted file as the findings name it. */
export const plantedName = path.relative(root, plantedFile);

/** Whether `finding` is the planted file's own: a line of that file, or a line of the tree that
 *  only planted lines print (a plant that prints what `shownSettingUrl` answers makes a line of
 *  `setting-url.ts` one). Such a line says nothing of the tree, and a judgement of it would hide
 *  every line of the tree that prints it too. */
export function plantedOwn(finding: Finding): boolean {
  return (
    finding.file === plantedName ||
    (finding.ends.length > 0 && finding.ends.every((end) => end.includes(plantedName)))
  );
}

/** The planted lines, by number, marked `found` or `quiet`. */
export function plantedMarks(text: string): { found: number[]; quiet: number[] } {
  const found: number[] = [];
  const quiet: number[] = [];
  text.split("\n").forEach((line, index) => {
    if (/\/\/ planted: found$/.test(line)) found.push(index + 1);
    if (/\/\/ planted: quiet$/.test(line)) quiet.push(index + 1);
  });
  return { found, quiet };
}

/** What the planted lines the TypeScript half found say that is not as marked: a line marked
 *  `found` that was not found, one marked `quiet` that was, and an unmarked one that was. */
export function plantedBlind(findings: readonly Finding[]): string[] {
  const marks = plantedMarks(PLANTED);
  const plantedFound = new Set(findings.filter((f) => f.file === plantedName).map((f) => f.line));
  return [
    ...marks.found
      .filter((line) => !plantedFound.has(line))
      .map((line) => `the planted line ${plantedName}:${line} was not found`),
    ...marks.quiet
      .filter((line) => plantedFound.has(line))
      .map((line) => `the planted line ${plantedName}:${line} was found`),
    ...[...plantedFound]
      .filter((line) => !marks.found.includes(line) && !marks.quiet.includes(line))
      .map((line) => `the planted line ${plantedName}:${line}, which is not marked, was found`),
  ];
}
