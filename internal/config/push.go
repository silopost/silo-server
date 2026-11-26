package config

// PushExpo is the push provider of GET /meta push.providers and of
// POST /devices: the Expo Push Service (silo-mobile).
const PushExpo = "expo"

// Official endpoints of the Expo Push Service.
const (
	ExpoSendURL     = "https://exp.host/--/api/v2/push/send"
	ExpoReceiptsURL = "https://exp.host/--/api/v2/push/getReceipts"
)

// PushConfig configures push notifications of native clients (phase 6).
// Push needs no keys: the server sends through the Expo Push Service and
// connects to it only once a device has registered. The *_URL fields only
// override the official endpoints for tests against a fake service; leave
// them empty in production.
type PushConfig struct {
	Enabled         bool   `env:"SILO_PUSH_ENABLED" envDefault:"true"`
	ExpoSendURL     string `env:"SILO_PUSH_EXPO_SEND_URL"`
	ExpoReceiptsURL string `env:"SILO_PUSH_EXPO_RECEIPTS_URL"`
}

// Providers lists the enabled push providers; never nil.
func (p PushConfig) Providers() []string {
	if !p.Enabled {
		return []string{}
	}
	return []string{PushExpo}
}

// SendURL is the Expo send endpoint (the override or the official one).
func (p PushConfig) SendURL() string { return orDefault(p.ExpoSendURL, ExpoSendURL) }

// ReceiptsURL is the Expo receipts endpoint (the override or the official one).
func (p PushConfig) ReceiptsURL() string { return orDefault(p.ExpoReceiptsURL, ExpoReceiptsURL) }
