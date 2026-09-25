package billing

import (
	"context"
	"maps"
	"sort"
	"time"
)

// referencePriceMaxAge is how old models.dev prices may get before a refresh.
// Zero reference_price_refresh_hours only disables the usage-time refresh;
// on-demand admission refreshes keep the historical one-day limit.
func (s *Store) referencePriceMaxAge() time.Duration {
	s.mu.RLock()
	hours := s.cfg.ReferencePriceRefreshHours
	s.mu.RUnlock()
	if hours <= 0 {
		hours = DefaultReferencePriceRefreshHours
	}
	return time.Duration(hours) * time.Hour
}

// MaybeRefreshReferencePrices refreshes models.dev prices when they are older
// than reference_price_refresh_hours. It runs synchronously inside
// usage.handle, which the host delivers from its own queue after the response,
// so no client waits for the download and the plugin starts no goroutines.
func (s *Store) MaybeRefreshReferencePrices() {
	s.mu.RLock()
	hours := s.cfg.ReferencePriceRefreshHours
	s.mu.RUnlock()
	references := s.referencePrices.Load()
	if hours <= 0 || references == nil {
		return
	}
	maxAge := time.Duration(hours) * time.Hour
	metadata, refreshSequence := references.status()
	now := s.Now()
	if metadata.Refreshing || now.Before(metadata.RetryAfter) || !referencePricesNeedRefresh(metadata, true, now, maxAge) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), referencePriceOperationTimeout)
	defer cancel()
	// Failures are recorded in the reference metadata and plugin log, and
	// RetryAfter backs off the next attempt.
	_, _ = s.refreshReferences(ctx, references, false, refreshSequence, true)
}

// refreshReferences refreshes reference prices and, when the models.dev data
// changed, updates custom prices that were unchanged copies of the previous
// reference price. Hand-edited custom prices are never modified.
func (s *Store) refreshReferences(ctx context.Context, references *referencePriceManager, force bool, observed uint64, found bool) (ReferencePriceMetadata, error) {
	var previous map[string]ReferencePrice
	var custom map[string]CustomPrice
	s.mu.RLock()
	syncCustom := s.cfg.SyncCustomPricesFromReference
	if syncCustom {
		custom = maps.Clone(s.state.Prices)
	}
	s.mu.RUnlock()
	before, _ := references.status()
	if syncCustom && len(custom) > 0 {
		// Lookups are local SQLite reads; a failure only skips syncing.
		previous, _ = references.lookupModels(ctx, customPriceModels(custom))
	}
	metadata, err := references.refresh(ctx, s.Now, force, observed, found, s.referencePriceMaxAge())
	if err != nil || metadata.Version == before.Version || len(previous) == 0 {
		return metadata, err
	}
	next, errLookup := references.lookupModels(ctx, customPriceModels(custom))
	if errLookup != nil {
		s.AddPluginLog(PluginLogError, "Failed to read updated reference prices for custom price sync: %v", errLookup)
		return metadata, err
	}
	s.syncCustomPrices(previous, next)
	return metadata, err
}

func customPriceModels(custom map[string]CustomPrice) []string {
	models := make([]string, 0, len(custom))
	for _, price := range custom {
		models = append(models, price.ModelID)
	}
	sort.Strings(models)
	return models
}

// syncCustomPrices replaces each custom price that still equals its previous
// reference rates with the new reference rates. The comparison is repeated
// under the store lock, so a concurrent manual edit always wins.
func (s *Store) syncCustomPrices(previous, next map[string]ReferencePrice) {
	updated := []string{}
	s.mu.Lock()
	for model, old := range previous {
		fresh, found := next[model]
		if old.PriceRates == nil || !found || fresh.PriceRates == nil || samePriceRates(*old.PriceRates, *fresh.PriceRates) {
			continue
		}
		key := NormalizeModelID(model)
		current, exists := s.state.Prices[key]
		if !exists || !samePriceRates(current.PriceRates, *old.PriceRates) {
			continue
		}
		replacement := CustomPrice{ModelID: current.ModelID, PriceRates: clonePriceRates(*fresh.PriceRates)}
		if replacement.Validate() != nil {
			continue
		}
		if s.repo != nil {
			if errUpsert := s.repo.UpsertPrice(replacement); errUpsert != nil {
				s.mu.Unlock()
				s.AddPluginLog(PluginLogError, "Failed to sync custom price for %s from models.dev: %v", current.ModelID, errUpsert)
				s.mu.Lock()
				continue
			}
		}
		s.state.Prices[key] = replacement
		updated = append(updated, current.ModelID)
	}
	s.mu.Unlock()
	sort.Strings(updated)
	for _, model := range updated {
		s.AddPluginLog(PluginLogInfo, "Updated custom price for %s from the new models.dev reference price", model)
	}
}

func clonePriceRates(rates PriceRates) PriceRates {
	clone := rates
	clone.CacheReadPer1M = cloneFloat(rates.CacheReadPer1M)
	clone.CacheWritePer1M = cloneFloat(rates.CacheWritePer1M)
	if rates.LongContext != nil {
		tier := *rates.LongContext
		tier.CacheReadPer1M = cloneFloat(tier.CacheReadPer1M)
		tier.CacheWritePer1M = cloneFloat(tier.CacheWritePer1M)
		clone.LongContext = &tier
	}
	return clone
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
