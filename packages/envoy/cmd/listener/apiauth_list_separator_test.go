package main

import (
	"strings"
	"testing"
)

// An account list written with commas between its entries, as ENVOY_ALLOWED_HOSTS beside it is
// written, never starts a listener that holds an entry no token can carry. Read whitespace-separated
// as it is today, "A, B" holds "A," and "B": the listener starts, and every caller A names is then
// refused one by one as not_listed, a refusal that names A, the account the operator can see in the
// list. The listener must either refuse the start, naming the setting, or read the comma as a
// separator; it must never hold an entry with a comma in it. (Acceptance of t-e-18 at 17da83d280:
// the binary started with ENVOY_OIDC_ACCOUNTS="system:serviceaccount:dispatch:dispatch,
// system:serviceaccount:envoy:envoy-broker" refused dispatch:dispatch's token 401 not_listed.)
func TestAnAccountListSeparatedByCommasHoldsNoEntryWithAComma(t *testing.T) {
	const issuer = "https://oidc.eks.example/id/CLUSTER"
	for _, value := range []string{
		listenerListedSubject + ", " + listenerUnlistedSubject,
		listenerListedSubject + ",",
		listenerListedSubject + " ,",
	} {
		accounts, err := resolveListenerOIDCAccounts(func(key string) string {
			if key == "ENVOY_OIDC_ACCOUNTS" {
				return value
			}
			return ""
		}, issuer)
		if err != nil {
			if !strings.Contains(err.Error(), "ENVOY_OIDC_ACCOUNTS") {
				t.Fatalf("ENVOY_OIDC_ACCOUNTS=%q: the refusal %q does not name the setting", value, err)
			}
			continue
		}
		for _, account := range accounts {
			if strings.Contains(account, ",") {
				t.Fatalf("ENVOY_OIDC_ACCOUNTS=%q starts a listener holding the entry %q, which no token can carry, so its caller is refused not_listed; want the start refused naming the setting, or the comma read as a separator", value, account)
			}
		}
	}
	for _, value := range []string{
		listenerGoogleAccount + ", " + listenerGoogleOtherEmail,
		listenerGoogleAccount + ",",
	} {
		_, accounts, err := resolveListenerGoogleConfig(func(key string) string {
			switch key {
			case "ENVOY_OIDC_GOOGLE_AUDIENCE":
				return listenerGoogleAudience
			case "ENVOY_OIDC_GOOGLE_ACCOUNTS":
				return value
			}
			return ""
		})
		if err != nil {
			if !strings.Contains(err.Error(), "ENVOY_OIDC_GOOGLE_ACCOUNTS") {
				t.Fatalf("ENVOY_OIDC_GOOGLE_ACCOUNTS=%q: the refusal %q does not name the setting", value, err)
			}
			continue
		}
		for _, account := range accounts {
			if strings.Contains(account, ",") {
				t.Fatalf("ENVOY_OIDC_GOOGLE_ACCOUNTS=%q starts a listener holding the entry %q, which no Google token can carry, so its caller is refused not_listed; want the start refused naming the setting, or the comma read as a separator", value, account)
			}
		}
	}
}
