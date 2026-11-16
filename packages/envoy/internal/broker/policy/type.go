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

// Each broker process reports the old tag once per secret, across loads and rereads.
var legacyTypesLogged sync.Map

func listingType(ls secrets.Listing) string {
	if _, present := ls.Tags[TagType]; !present {
		if _, legacy := ls.Tags[LegacyTypeTag]; legacy {
			if _, logged := legacyTypesLogged.LoadOrStore(ls.ID, struct{}{}); !logged {
				slog.Warn("agent secret uses legacy classifier tag; retag it with --type", "name", ls.ID, "old_tag", LegacyTypeTag, "tag", TagType)
			}
		}
	}
	return TypeFromTags(ls.Tags)
}
