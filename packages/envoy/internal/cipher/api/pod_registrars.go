package api

import "slices"

// PodRegistrar is one BROKER_POD_REGISTRARS entry: an account that may register pods at POST
// /v1/oauth/exchange/pod-sessions, and the account of the pods it may register there, each a
// ServiceAccount subject (system:serviceaccount:<namespace>:<name>).
type PodRegistrar struct {
	// Registrar is the account the registrar logs in as on that route (serviceAuth), the
	// subject of its own projected token.
	Registrar string
	// Pod is the subject of the pod token a registration's body carries.
	Pod string
}

// PodRegistrars is BROKER_POD_REGISTRARS as the registration route reads it: the set of pairs it
// admits, each one entry of the setting (config.Load refuses a pair given twice). It is the
// registration's authority, where BROKER_SERVICE_LOGINS only lets an account call the route at
// all. A registrar may be paired with several pod accounts and a pod account with several
// registrars; an account paired with itself is a daemon registering the sibling pods of its own
// account. The nil set, the setting unset, lists no pair, so the route admits no registration.
type PodRegistrars map[PodRegistrar]struct{}

// Lists reports whether an entry pairs registrar, the account a caller of the registration route
// logs in as, with pod, the account of the pod it registers.
func (p PodRegistrars) Lists(registrar, pod string) bool {
	_, listed := p[PodRegistrar{Registrar: registrar, Pod: pod}]
	return listed
}

// ListsRegistrar reports whether an entry names registrar as its registrar, for any pod account:
// the operator's move binds a session name only to such an account, since a name bound to any
// other would admit no registration of it at all.
func (p PodRegistrars) ListsRegistrar(registrar string) bool {
	for pair := range p {
		if pair.Registrar == registrar {
			return true
		}
	}
	return false
}

// Registrars is every registrar account an entry names, sorted and each once, for the boot's
// line naming a registrar BROKER_SERVICE_LOGINS does not list.
func (p PodRegistrars) Registrars() []string {
	registrars := make([]string, 0, len(p))
	for pair := range p {
		registrars = append(registrars, pair.Registrar)
	}
	slices.Sort(registrars)
	return slices.Compact(registrars)
}
