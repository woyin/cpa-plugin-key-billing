package billing

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUsageRefreshHonorsInterval(t *testing.T) {
	raw := referencePricesJSON(t)
	for _, tc := range []struct {
		name      string
		age       time.Duration
		hours     int
		downloads int
	}{
		{"fresh", 23 * time.Hour, 24, 0},
		{"stale", 25 * time.Hour, 24, 1},
		{"short interval", 2 * time.Hour, 1, 1},
		{"disabled", 30 * 24 * time.Hour, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newReferencePriceStore(t, tc.age)
			s.cfg.ReferencePriceRefreshHours = tc.hours
			calls := 0
			referencePriceServer(t, s, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_, _ = w.Write(raw)
			})
			s.MaybeRefreshReferencePrices()
			s.MaybeRefreshReferencePrices() // the second call sees fresh prices
			if calls != tc.downloads {
				t.Fatalf("downloads = %d, want %d", calls, tc.downloads)
			}
		})
	}
}

func TestUsageRefreshRespectsRetryBackoff(t *testing.T) {
	s, _ := newReferencePriceStore(t, 25*time.Hour)
	calls := 0
	referencePriceServer(t, s, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	s.MaybeRefreshReferencePrices()
	s.MaybeRefreshReferencePrices()
	if calls != 1 {
		t.Fatalf("a failed refresh must back off before retrying, downloads = %d", calls)
	}
}

// updatedReferenceJSON raises gpt-4o and gemini-2.0-flash reference prices.
func updatedReferenceJSON(t *testing.T) []byte {
	raw := string(referencePricesJSON(t))
	raw = strings.Replace(raw, `{"input":5,"output":15,"cache_read":2.5}`, `{"input":6,"output":18,"cache_read":3}`, 1)
	raw = strings.Replace(raw, `{"input":0.1,"output":0.4}`, `{"input":0.15,"output":0.6}`, 1)
	return []byte(raw)
}

func copyReferencePrice(t *testing.T, s *Store, model string) CustomPrice {
	t.Helper()
	matched, err := s.referencePrices.Load().lookupModels(context.Background(), []string{model})
	if err != nil || matched[model].PriceRates == nil {
		t.Fatalf("reference price for %s: %v", model, err)
	}
	price, err := s.UpsertPrice(CustomPrice{ModelID: model, PriceRates: clonePriceRates(*matched[model].PriceRates)})
	if err != nil {
		t.Fatal(err)
	}
	return price
}

func TestCustomPriceCopiesFollowReferenceUpdates(t *testing.T) {
	s, _ := newReferencePriceStore(t, time.Hour)
	copyReferencePrice(t, s, "gpt-4o")
	// A hand-edited price differs from the reference and must be preserved.
	handEdited := CustomPrice{ModelID: "gemini-2.0-flash", PriceRates: PriceRates{InputPer1M: 0.2, OutputPer1M: 0.5}}
	if _, err := s.UpsertPrice(handEdited); err != nil {
		t.Fatal(err)
	}
	// A custom-only model has no reference and is untouched.
	if _, err := s.UpsertPrice(CustomPrice{ModelID: "private-model", PriceRates: PriceRates{InputPer1M: 1, OutputPer1M: 2}}); err != nil {
		t.Fatal(err)
	}
	updated := updatedReferenceJSON(t)
	referencePriceServer(t, s, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(updated) })

	result, err := s.RefreshReferencePrices()
	if err != nil || !result.Changed {
		t.Fatal(result, err)
	}
	price, _, err := s.ResolveModelPrice("gpt-4o", "gpt-4o", false)
	if err != nil || price.Source != PriceSourceCustom || price.InputPer1M != 6 || price.OutputPer1M != 18 || price.CacheReadPer1M != 3 {
		t.Fatalf("copied custom price did not follow models.dev: %+v, %v", price, err)
	}
	price, _, _ = s.ResolveModelPrice("gemini-2.0-flash", "gemini-2.0-flash", false)
	if price.InputPer1M != 0.2 || price.OutputPer1M != 0.5 {
		t.Fatalf("hand-edited custom price was overwritten: %+v", price)
	}
	price, _, _ = s.ResolveModelPrice("private-model", "private-model", false)
	if price.InputPer1M != 1 || price.OutputPer1M != 2 {
		t.Fatalf("custom-only price changed: %+v", price)
	}
}

func TestCustomPriceSyncCanBeDisabled(t *testing.T) {
	s, _ := newReferencePriceStore(t, time.Hour)
	s.cfg.SyncCustomPricesFromReference = false
	copyReferencePrice(t, s, "gpt-4o")
	updated := updatedReferenceJSON(t)
	referencePriceServer(t, s, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(updated) })
	if _, err := s.RefreshReferencePrices(); err != nil {
		t.Fatal(err)
	}
	price, _, _ := s.ResolveModelPrice("gpt-4o", "gpt-4o", false)
	if price.InputPer1M != 5 || price.OutputPer1M != 15 {
		t.Fatalf("custom price changed although sync is disabled: %+v", price)
	}
}

func TestUsageRefreshAlsoSyncsCustomPrices(t *testing.T) {
	s, _ := newReferencePriceStore(t, 25*time.Hour)
	copyReferencePrice(t, s, "gpt-4o")
	updated := updatedReferenceJSON(t)
	referencePriceServer(t, s, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(updated) })
	s.MaybeRefreshReferencePrices()
	price, _, _ := s.ResolveModelPrice("gpt-4o", "gpt-4o", false)
	if price.Source != PriceSourceCustom || price.InputPer1M != 6 {
		t.Fatalf("usage-time refresh did not sync the copied price: %+v", price)
	}
}
