package billing

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultStateFile = "plugins/cpa-key-billing-state-v1.db"

// Policies for requests whose model has no custom, builtin or reference price.
const (
	// UnpricedModelsAllow admits the request and bills it at zero cost. Token
	// and request quotas still apply; the model is reported in plugin logs.
	UnpricedModelsAllow = "allow"
	// UnpricedModelsBlock refuses the request with model_price_error.
	UnpricedModelsBlock = "block"
)

// DefaultReferencePriceRefreshHours is how old the models.dev reference
// prices may get before usage handling refreshes them.
const DefaultReferencePriceRefreshHours = 24

type Config struct {
	Enabled               bool   `yaml:"enabled"`
	Debug                 bool   `yaml:"debug"`
	StateFile             string `yaml:"state_file"`
	CodexFastModeBilling  bool   `yaml:"codex_fast_mode_billing"`
	MaskAPIKeyViewEmails  bool   `yaml:"mask_api_key_view_emails"`
	AllowAPIKeyQuotaReset bool   `yaml:"allow_api_key_quota_reset"`
	// UnpricedModels is UnpricedModelsAllow or UnpricedModelsBlock.
	UnpricedModels string `yaml:"unpriced_models"`
	// ReferencePriceRefreshHours refreshes models.dev prices during usage
	// handling once they are older than this. Zero keeps only the on-demand
	// refresh for models that have no custom price.
	ReferencePriceRefreshHours int `yaml:"reference_price_refresh_hours"`
	// SyncCustomPricesFromReference updates custom prices that are unchanged
	// copies of the previous reference price whenever models.dev changes.
	SyncCustomPricesFromReference bool `yaml:"sync_custom_prices_from_reference"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:                       false,
		StateFile:                     DefaultStateFile,
		UnpricedModels:                UnpricedModelsAllow,
		ReferencePriceRefreshHours:    DefaultReferencePriceRefreshHours,
		SyncCustomPricesFromReference: true,
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		document := struct {
			Config `yaml:",inline"`
			// These fields belong to the host and are ignored by the plugin.
			Priority int       `yaml:"priority"`
			Store    yaml.Node `yaml:"store"`
		}{Config: cfg}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if errDecode := decoder.Decode(&document); errDecode != nil {
			return Config{}, fmt.Errorf("Parse plugin configuration: %w", errDecode)
		}
		if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
			return Config{}, fmt.Errorf("Plugin configuration must contain exactly one YAML document")
		}
		cfg = document.Config
		switch strings.ToLower(strings.TrimSpace(cfg.UnpricedModels)) {
		case "", UnpricedModelsAllow, UnpricedModelsBlock:
		default:
			return Config{}, fmt.Errorf("Parse plugin configuration: unpriced_models must be %q or %q", UnpricedModelsAllow, UnpricedModelsBlock)
		}
		if cfg.ReferencePriceRefreshHours < 0 {
			return Config{}, fmt.Errorf("Parse plugin configuration: reference_price_refresh_hours must not be negative")
		}
	}
	return cfg.normalized(), nil
}

func (c Config) describe() string {
	state := "disabled"
	if c.Enabled {
		state = "enabled"
	}
	return fmt.Sprintf("%s, unpriced_models=%s, reference_price_refresh_hours=%d, sync_custom_prices_from_reference=%t",
		state, c.UnpricedModels, c.ReferencePriceRefreshHours, c.SyncCustomPricesFromReference)
}

func (c Config) normalized() Config {
	c.StateFile = strings.TrimSpace(c.StateFile)
	if c.StateFile == "" {
		c.StateFile = DefaultStateFile
	}
	switch strings.ToLower(strings.TrimSpace(c.UnpricedModels)) {
	case UnpricedModelsBlock:
		c.UnpricedModels = UnpricedModelsBlock
	default:
		c.UnpricedModels = UnpricedModelsAllow
	}
	c.ReferencePriceRefreshHours = max(c.ReferencePriceRefreshHours, 0)
	return c
}
