package policy

import (
	"log/slog"
	"sync"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// LegacyTypeTag is read for one release when TagType is absent. Writers use TagType.
const LegacyTypeTag = "kind"

// TypeFromTags reads the classifier, with an explicitly empty type taking precedence too.
func TypeFromTags(tags map[string]string) string {
	if value, present := tags[TagType]; present {
		return value
	}
	return tags[LegacyTypeTag]
}

// LegacyTypeTagMessage is the line Load and LoadOne write, at WARN, the first time a broker
// process reads a secret's type from LegacyTypeTag because it carries no TagType: name (the
// secret's id in the store), old_tag and tag. The operate guide documents its text, so the text
// changes only with that guide.
const LegacyTypeTagMessage = "agent secret uses legacy classifier tag; retag it with --type"

// legacyTypesLogged is every secret id this broker process has written LegacyTypeTagMessage for,
// across loads and rereads, so each secret is reported once per process.
var legacyTypesLogged sync.Map

func listingType(ls secrets.Listing) string {
	if _, present := ls.Tags[TagType]; !present {
		if _, legacy := ls.Tags[LegacyTypeTag]; legacy {
			if _, logged := legacyTypesLogged.LoadOrStore(ls.ID, struct{}{}); !logged {
				slog.Warn(LegacyTypeTagMessage, "name", ls.ID, "old_tag", LegacyTypeTag, "tag", TagType)
			}
		}
	}
	return TypeFromTags(ls.Tags)
}
