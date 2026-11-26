import { pageMutedText, pageQuietText } from "../../theme/classes";

/** The page's colours of a question's Reaches control in Details' facts (R13): its words and its
 *  chevron, which `inbox/AskRecipients.tsx` draws where its palette is the page's. They live here,
 *  beside the page, so that file keeps `main`'s import of its own colours unchanged. */
export const PAGE_REACHES = { chevron: pageQuietText, text: pageMutedText } as const;
