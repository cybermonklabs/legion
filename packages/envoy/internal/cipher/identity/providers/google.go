//go:build !cipher_no_google

package providers

import (
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
)

func init() {
	service := google.ServiceTokenSource()
	register(entry{
		kind: google.Kind, rank: 20,
		sources: google.Sources(),
		detectors: []identity.Detector{{
			Provider:      google.Kind,
			SettingsField: "google_exchange_audience",
			KeptFile:      "google-exchange-audience",
			Words:         "Google exchange audience",
			Detect:        google.DetectMachineIdentity,
		}},
		serviceToken: &service,
		actorType:    google.ActorTokenType,
		personType:   google.PersonTokenType,
	})
}
