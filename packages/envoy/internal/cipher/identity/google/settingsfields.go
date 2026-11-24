package google

// SettingsFields is what the google provider adds to GET /v1/settings.
type SettingsFields struct {
	// ExchangeAudience is the Google audience a machine mints its identity token for to trade at
	// the token exchange: BROKER_GOOGLE_OIDC_AUDIENCE where BROKER_VM_SERVICES lists a machine
	// the exchange may mint a launcher credential for, and empty where the audience is set for an
	// API caller alone (a google: entry in BROKER_API_ACCOUNTS). The exchange refuses every
	// machine then, and a client that read the audience would mint a token, be refused and stop
	// on every command, where with none published it skips the machine source.
	ExchangeAudience string `json:"google_exchange_audience,omitempty"`
}

// Settings is what this provider adds to GET /v1/settings.
func (p *Provider) Settings() SettingsFields {
	if len(p.config.Machines) == 0 {
		return SettingsFields{}
	}
	return SettingsFields{ExchangeAudience: p.config.Audience}
}
