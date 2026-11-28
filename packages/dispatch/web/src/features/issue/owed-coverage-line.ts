/**
 * The line the coverage read writes for a document of the issue that owes its block index, in
 * place of every missing check and plan task while it does (the server's `owedGap`): `<file
 * name> version <n> is still being indexed; its coverage shows once that ends.`. It names a file,
 * so it is drawn in the server's own words. It stands in a module of its own, which imports
 * nothing, so today's Spec tab (`SpecRequirements`) and the new issue page's words (`page-words`)
 * read one test, while today's pages load no file of the new page's model.
 */
export const OWED_COVERAGE_LINE =
  /^\S.* version \d+ is still being indexed; its coverage shows once that ends\.$/u;
