package pmdoc

// SetBlockSchemaForTest makes value the block schema, as if the server had been built with it:
// every reader of this package, SchemaVersion, LinkKinds and the bytes SchemaJSON serves. It
// refuses value when it is not a valid schema, and changes nothing then. restore puts back the
// schema in use before. Tests reach it through pmdoctest.UseBlockSchema, which says when a test
// may call it.
func SetBlockSchemaForTest(value BlockSchema) (restore func(), err error) {
	previous := installed.Load()
	if err := installSchema(value); err != nil {
		return nil, err
	}
	return func() { installed.Store(previous) }, nil
}
