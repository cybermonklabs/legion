import { pageLineBorder, pageMutedText } from "../../theme/classes";

/** The reply quote's colours on the new issue page (`ReplyQuote`'s `palette`): the page's line
 *  rule and its muted words, as the page's own composer draws the post it answers
 *  (`pageComposer.target`), and the link turning to the ink under the pointer. They stand in a
 *  module of their own so that `ReplyQuote.tsx` keeps `main`'s import of the theme's classes as
 *  it is (spec R26): Biome's import sorting merges a second import of one module into the first. */
export const PAGE_QUOTE = {
  link: "hover:text-page-ink",
  rule: `${pageLineBorder} ${pageMutedText}`,
} as const;
