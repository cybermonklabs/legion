package main

// privacy_readiness.go is GET /v1/privacy-readiness: the proof Dispatch's hub admission
// (busprojects.HubAdmission.Prove, plan C's t-c-11) reads from every serving listener replica
// before a project may be switched private. It is answered only to a caller of
// ENVOY_ALL_PROJECTS_ACCOUNTS, Dispatch's own service account; any other caller gets 403.
//
// Every call makes its own fresh read of the privacy source through K7's one reader (k7Reader),
// the reader the relay, the routes, the role fallback and the Slack intake decide by, with no
// cache and no join of a read begun before the call. The answer carries no project and no channel:
//
//	required mode, the read answered:  200 {"instance":"<host>","required":true,"source":"<ENVOY_PROJECT_PRIVACY_URL>","read":"answered"}
//	required mode, any other outcome:  503 {"instance":"<host>","required":true,"source":"<ENVOY_PROJECT_PRIVACY_URL>","read":"failed"}
//	all-public mode:                   200 {"instance":"<host>","required":false,"source":"","read":"none"}

import (
	"net/http"
	"os"
)

// The read values of a readiness answer.
const (
	readinessAnswered = "answered"
	readinessFailed   = "failed"
	readinessNone     = "none"
)

// privacyReadiness is GET /v1/privacy-readiness's answer.
type privacyReadiness struct {
	Instance string `json:"instance"`
	Required bool   `json:"required"`
	Source   string `json:"source"`
	Read     string `json:"read"`
}

// privacyReadinessHandler answers GET /v1/privacy-readiness over privacy.
func privacyReadinessHandler(privacy *listenerPrivacy) http.HandlerFunc {
	instance, _ := os.Hostname()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !scopeOf(r, privacy).allProjects {
			writeJSONError(w, http.StatusForbidden, "the privacy readiness of this listener is answered only to "+EnvAllProjectsAccounts)
			return
		}
		if !privacy.isRequired() {
			writeJSON(w, http.StatusOK, privacyReadiness{Instance: instance, Read: readinessNone})
			return
		}
		answer := privacyReadiness{Instance: instance, Required: true, Source: privacy.k7.source, Read: readinessAnswered}
		if _, err := privacy.k7.Read(r.Context()); err != nil {
			answer.Read = readinessFailed
			writeJSON(w, http.StatusServiceUnavailable, answer)
			return
		}
		writeJSON(w, http.StatusOK, answer)
	}
}
