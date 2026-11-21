package redact

// AnswerBytes is control (30)'s planted function that hands an answer's bytes out, as a method
// would.
func AnswerBytes(a Answer) []byte { return a.bytes() }
