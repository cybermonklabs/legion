package docs

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// LostOptionWords weighs a rewrite of the options of a decision block that is not written yet by
// the ask-edit route's own refusal (refuseLostOptionWords), so the reviewer of INFRA-3231 can say
// that the rewrite would not be applied before it tries to write it: markdown is the document,
// blockID the decision block, and options the rewrite's options. It answers the refusal, an
// *ErrAskBlockUnrepresentable on the options, where the write would delete words only the
// document holds; nil where it would delete none; and an error where markdown holds no decision
// block blockID that the ask-edit route can read.
func LostOptionWords(markdown, blockID string, options []model.AskOption) error {
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		return fmt.Errorf("parse the document: %w", err)
	}
	block, _, err := askBlockToEdit(tree, blockID)
	if err != nil {
		return err
	}
	return refuseLostOptionWords(block, AskBlockEdit{Options: &options})
}
