/**
 * A source file with its comments taken out and its lines left where they were.
 *
 * Every guard that reads the repository as text has the same problem: it has to describe the
 * marks it refuses in prose, and prose sits in the very files it scans, so a guard that read
 * its own comments would refuse the code that documents it. Stripping first is what lets each
 * one say what it looks for (`theme/no-raw-colors.test.ts`,
 * `components/one-definition.test.ts`).
 *
 * A block comment's text goes and its newlines stay, so a line number read off the result is
 * the line number in the file the reader has to open.
 */
export function stripComments(source: string): string {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, (comment) => comment.replace(/[^\n]/g, ""))
    .replace(/\/\/.*$/gm, "");
}
