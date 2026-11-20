package redact

import "strings"

// publicRequestName is one name whose value a wire contract declares public: Place is "header" or
// "field" (a query, form or JSON body field), and Source is that contract. The table,
// publicRequestNames, is generated from packages/contracts/src/public-request-names.ts, which the
// TypeScript half reads, so both halves read one table.
type publicRequestName struct {
	Place, Name, Source string
}

// publicHeader reports whether a header named name carries a value public by its contract, the
// name compared without case, as HTTP compares it.
func publicHeader(name string) bool {
	for _, entry := range publicRequestNames {
		if entry.Place == "header" && strings.EqualFold(entry.Name, name) {
			return true
		}
	}
	return false
}

// publicField reports whether a query, form or JSON body field named name carries a value public
// by its contract.
func publicField(name string) bool {
	for _, entry := range publicRequestNames {
		if entry.Place == "field" && entry.Name == name {
			return true
		}
	}
	return false
}
