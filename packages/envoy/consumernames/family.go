package consumernames

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// Naming is how a family names the consumers of a logical name: Prefix(logical) begins each of its
// fresh names (Prefix(logical)~<random>), Fixed(logical) is the name the family gave logical before
// this package, which the cutover's convert replaces, Metadata(logical) is the metadata every
// consumer of logical carries, which its create writes and the owner check requires, and
// CheckKey(logical) refuses a key that is none of the family's. Every Naming the table gives sets
// all four. Each creator builds its Config's Prefix, Fixed, Metadata and CheckKey from its family's
// Naming (FamilyNaming), and CheckFamily and OwnsFamily judge a key, a record and a consumer by the
// same, so the creators and an operator's reader, which may not import the creators' packages,
// hold one table of the families' names.
type Naming struct {
	Prefix   func(logical string) string
	Fixed    func(logical string) string
	Metadata func(logical string) map[string]string
	CheckKey func(logical string) error
}

const (
	// SessionPrefix begins every fresh session consumer's name: session~<random>, for every session.
	SessionPrefix = "session"
	// SessionFixedPrefix begins every fixed session consumer's name: s-<session id>.
	SessionFixedPrefix = "s-"
	// SessionIDMetadataKey is the metadata every session consumer of the fresh mode carries, naming
	// its session, beside LogicalMetadataKey.
	SessionIDMetadataKey = "envoy.session_id"
	// DispatchDeliveryDurable and DispatchJobDurable are Dispatch's two durables (its delivery
	// intake's and its job intake's), the only keys of Dispatch's store.
	DispatchDeliveryDurable = "delivery-events"
	DispatchJobDurable      = "delivery-jobs"
	// daemonDurablePrefix begins every durable of a Legion daemon: legion-go-<project>-<kind>.
	daemonDurablePrefix = "legion-go-"
	// ListenerDurablePrefix begins the name of every listener's own durable,
	// listener-<machine id> (bus.ListenerDurable): the only durables the session store keeps, under
	// DurableKey.
	ListenerDurablePrefix = "listener-"
)

// SessionNaming is the session family's naming, for a session's key (its id): its fresh names are
// session~<random> (SessionPrefix), its fixed name s-<session id> (SessionFixedPrefix), each of its
// consumers carries the session id under LogicalMetadataKey and SessionIDMetadataKey, and its keys
// are the ones CheckKey passes.
func SessionNaming() Naming {
	return Naming{
		Prefix: func(string) string { return SessionPrefix },
		Fixed:  func(sessionID string) string { return SessionFixedPrefix + sessionID },
		Metadata: func(sessionID string) map[string]string {
			return map[string]string{LogicalMetadataKey: sessionID, SessionIDMetadataKey: sessionID}
		},
		CheckKey: CheckKey,
	}
}

// DurableNaming is the naming of a durable kept in the session store under DurableKey(durable),
// the listener's own: its fresh names are <durable>~<random>, its fixed name the durable's name, each
// of its consumers carries the durable's name under LogicalMetadataKey, and its keys are the ones
// DurableKey makes of a listener's durable (checkListenerDurableKey), so a key it did not make has
// no durable, and a key of a daemon's or Dispatch's durable none of this family's.
func DurableNaming() Naming {
	durable := func(key string) string {
		durable, _ := durableOfKey(key)
		return durable
	}
	return Naming{Prefix: durable, Fixed: durable, Metadata: func(key string) map[string]string {
		return map[string]string{LogicalMetadataKey: durable(key)}
	}, CheckKey: checkListenerDurableKey}
}

// checkListenerDurableKey refuses, as ErrInvalidKey naming it, a key DurableKey did not make
// (CheckDurableKey) and one DurableKey made of a durable that is no listener's own
// (ListenerDurablePrefix and a machine id): the session store keeps the listeners' durables alone,
// so the key of a daemon's or Dispatch's durable there would name that family's durable as the
// key's own fixed name, which the fence and the owner check then serve.
func checkListenerDurableKey(key string) error {
	if err := CheckDurableKey(key); err != nil {
		return err
	}
	if durable, _ := durableOfKey(key); len(durable) <= len(ListenerDurablePrefix) || !strings.HasPrefix(durable, ListenerDurablePrefix) {
		return fmt.Errorf("%w: %q is the key of the durable %q, no listener's (%s<machine id>)", ErrInvalidKey, key, durable, ListenerDurablePrefix)
	}
	return nil
}

// LogicalNaming is the naming of a durable whose logical name is its own, Dispatch's and the
// daemons': its fresh names are <logical>~<random>, its fixed name the logical name, each of its
// consumers carries the logical name under LogicalMetadataKey, and its keys are the ones CheckKey
// passes. FamilyNaming narrows the keys to the family's own: Dispatch's two durables, or a
// daemon's legion-go-<project>-<kind>.
func LogicalNaming() Naming {
	logical := func(logical string) string { return logical }
	return Naming{Prefix: logical, Fixed: logical, Metadata: func(logical string) map[string]string {
		return map[string]string{LogicalMetadataKey: logical}
	}, CheckKey: CheckKey}
}

// daemonKinds are the kinds of durable a Legion daemon opens for its project,
// legion-go-<project>-<kind>: its intake's dispatch, github, wake and ready consumers.
var daemonKinds = []string{"dispatch", "github", "wake", "ready"}

// checkDispatchKey refuses, as ErrInvalidKey naming it, a key of Dispatch's store that is neither of
// Dispatch's durables.
func checkDispatchKey(logical string) error {
	if logical != DispatchDeliveryDurable && logical != DispatchJobDurable {
		return fmt.Errorf("%w: %q is no key of the store of %s, whose keys are %s and %s", ErrInvalidKey, logical, DispatchFamily, DispatchDeliveryDurable, DispatchJobDurable)
	}
	return nil
}

// daemonKey is the key rule of the store of project's daemon: legion-go-<project>-<kind> for a kind
// of daemonKinds, which CheckKey passes; any other key, another project's durable among them, is
// ErrInvalidKey naming it.
func daemonKey(project string) func(logical string) error {
	return func(logical string) error {
		for _, kind := range daemonKinds {
			if logical == daemonDurablePrefix+project+"-"+kind {
				return CheckKey(logical)
			}
		}
		return fmt.Errorf("%w: %q is no key of the store of %s%s, whose keys are %s%s-<kind> for the kinds %s",
			ErrInvalidKey, logical, daemonFamilyPrefix, project, daemonDurablePrefix, project, strings.Join(daemonKinds, ", "))
	}
}

// FamilyNaming is the naming of logical's consumers in the store of family: in the session family's
// store, DurableNaming for a key with DurableKey's prefix, over a listener's durable alone, and
// SessionNaming for any other; in Dispatch's, LogicalNaming over Dispatch's two durables alone; in a
// daemon's (DaemonFamily), LogicalNaming over legion-go-<project>-<kind> of its own project alone. A
// family no store keeps is ErrInvalidFamily, and a key the family's CheckKey refuses ErrInvalidKey,
// each naming it: a key of another family's (the listener's durable, another project's daemon, the
// DurableKey of a daemon's or Dispatch's durable) or no key at all is no key of family's store,
// however its record reads.
func FamilyNaming(family, logical string) (Naming, error) {
	if err := CheckStoreFamily(family); err != nil {
		return Naming{}, err
	}
	var naming Naming
	switch project, daemon := strings.CutPrefix(family, daemonFamilyPrefix); {
	case daemon:
		naming = LogicalNaming()
		naming.CheckKey = daemonKey(project)
	case family == DispatchFamily:
		naming = LogicalNaming()
		naming.CheckKey = checkDispatchKey
	case strings.HasPrefix(logical, durableKeyPrefix):
		naming = DurableNaming()
	default:
		naming = SessionNaming()
	}
	if err := naming.CheckKey(logical); err != nil {
		return Naming{}, err
	}
	return naming, nil
}

// CheckFamily is the family fence (Names.check, the one each creator applies to every record it
// reads) of logical's record in the store of family, by the family's own naming (FamilyNaming), for
// a reader with no creator's Config: an operator's status, census or cleanup. It answers what the
// family's creator answers of the record. A key that is none of the family's is ErrInvalidKey,
// naming it, before the record is judged (FamilyNaming): the fence judges a record's names by its
// key's own prefix, so it cannot tell a record keyed by another family's logical name from one of
// family's own.
//
// Its nil says only that the record's names are of logical's family. It says nothing of the record
// itself, which must come from a store read or DecodeKVValue (its own key, well-formed fields), nor
// of the consumers: every session's prefix is "session", so a record of one session passes with
// another session's consumer as its current. A name the record gives must pass the owner check
// before anything reads, binds, updates or deletes it, as the creator does.
func CheckFamily(family, logical string, record Record) error {
	naming, err := FamilyNaming(family, logical)
	if err != nil {
		return err
	}
	return naming.names().check(logical, record)
}

// OwnsFamily is the owner check (Names.Owns, the one each creator applies before it reads, binds,
// updates, adopts or deletes a consumer a record names) of info, a consumer a record of logical
// names in the store of family, by the family's own naming and metadata (FamilyNaming), for a
// reader with no creator's Config: nil when the consumer is logical's own (its metadata holds each
// pair the family's consumers carry) or logical's fixed name, else ErrMalformedRecord naming the
// consumer and the pair it lacks. A family no store keeps is ErrInvalidFamily, and a key that is
// none of the family's ErrInvalidKey, naming it, so no key of another family's store makes its
// consumer look owned here. A reader that deletes a consumer a record names asks here first, as
// the creator does.
func OwnsFamily(family, logical string, info *jetstream.ConsumerInfo) error {
	naming, err := FamilyNaming(family, logical)
	if err != nil {
		return err
	}
	return naming.names().Owns(logical, info)
}

// names is the Names a reader with no creator's Config judges a family's keys, records and
// consumers by: the family's naming, metadata and key rule, and nothing else.
func (naming Naming) names() *Names {
	return &Names{cfg: Config{Prefix: naming.Prefix, Fixed: naming.Fixed, Metadata: naming.Metadata, CheckKey: naming.CheckKey}}
}

// Naming is n's own naming (Config.Prefix, Config.Fixed, Config.Metadata and Config.CheckKey),
// which a test of a family's creator holds equal to the family's (FamilyNaming).
func (n *Names) Naming() Naming {
	return Naming{Prefix: n.cfg.Prefix, Fixed: n.cfg.Fixed, Metadata: n.cfg.Metadata, CheckKey: n.cfg.CheckKey}
}

// durableOfKey is the durable whose key DurableKey made key, and false for a key it did not.
func durableOfKey(key string) (string, bool) {
	escaped, ok := strings.CutPrefix(key, durableKeyPrefix)
	if !ok || escaped == "" {
		return "", false
	}
	var durable strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] != '=' {
			durable.WriteByte(escaped[i])
			continue
		}
		if i+2 >= len(escaped) {
			return "", false
		}
		b, err := strconv.ParseUint(escaped[i+1:i+3], 16, 8)
		if err != nil {
			return "", false
		}
		durable.WriteByte(byte(b))
		i += 2
	}
	return durable.String(), true
}
