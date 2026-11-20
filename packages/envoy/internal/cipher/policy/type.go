package policy

import (
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TypeFromTags reads the classifier from TagType alone.
func TypeFromTags(tags map[string]string) string {
	return tags[TagType]
}

// LegacyTypeTagMessage is the line Load and LoadOne write, at WARN, the first time a broker
// process reads a secret's type from LegacyTypeTag because it carries no TagType: name (the
// secret's id in the store), old_tag and tag. The operate guide documents its text, so the text
// changes only with that guide.
const LegacyTypeTagMessage = "agent secret uses legacy classifier tag; retag it with --type"


func listingType(ls secrets.Listing) string {
	return TypeFromTags(ls.Tags)
}
