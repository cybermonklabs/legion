package reviewer

// The written rules of INFRA-3231's R6 are skills/plain-words (plainwords.Rules), which the
// dispatch command shows an agent before a write, and the rule sets a deployment shows beside them
// (the api package's POST /api/v1/writing-rules, DISPATCH_WRITING_RULES_EXTRA): two of this
// package's rule sets, reader_words and ask_parts, written for the writer. Their text is a prompt
// of this package (prompts/written-rules.md), so an ask's parts read the same in the written rules,
// in the reviewer's ask_parts rule set and in a rewrite's side question (prompts/rules.md,
// "ask-parts").

// WrittenReaderWords is the written rules' section of the words reader asked about before, words,
// in their order: the reader_words rule set, for the writer.
func WrittenReaderWords(reader string, words []string) (string, error) {
	return render("written-reader-words", struct {
		Reader string
		Words  []string
	}{reader, words})
}

// WrittenAskParts is the written rules' section of the parts of an ask: the ask_parts rule set,
// for the writer.
func WrittenAskParts() (string, error) {
	return render("written-ask-parts", nil)
}
