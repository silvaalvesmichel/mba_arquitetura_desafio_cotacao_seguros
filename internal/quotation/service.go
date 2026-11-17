package quotation

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type Quoter interface {
	Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error)
}

// Span attributes. None of them carries a document, a plate or a quote_id.
const (
	SpanPartnerQuote = "partner.quote"

	AttrPartner     = "partner"
	AttrTenant      = "tenant_id"
	AttrCacheResult = "quotation.cache.result"
	AttrDegraded    = "quotation.degraded"
	AttrCacheHits   = "quotation.cache.hits"
)

const tracerName = "github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation"

type Service struct {
	partners []platform.Partner
	quoter   Quoter

	cache    Cache
	cacheTTL time.Duration
	partial  bool
	now      func() time.Time
	metrics  *metrics
}

type Option func(*Service)

// WithCache serves a repeated quote (same broker, same partner, same risk) from the cache for at most
// ttl, and never for longer than the validity the partner declared.
func WithCache(cache Cache, ttl time.Duration) Option {
	return func(s *Service) { s.cache, s.cacheTTL = cache, min(ttl, MaxCacheTTL) }
}

// WithPartialResponses is the fallback: a partner that cannot answer no longer fails the request.
// The broker gets the quotes that did arrive, flagged as degraded, with the missing partners named.
func WithPartialResponses() Option {
	return func(s *Service) { s.partial = true }
}

// WithClock replaces time.Now, so cache age and expiry can be tested without sleeping.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

func NewService(partners []platform.Partner, quoter Quoter, options ...Option) *Service {
	s := &Service{partners: partners, quoter: quoter, now: time.Now, metrics: newMetrics()}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Service) Quote(ctx context.Context, tenant string, request Request) (Response, error) {
	start := time.Now()
	forPartner := partnerRequest{Broker: tenant, Request: request}

	quotes := make([]Quote, 0, len(s.partners))
	var unavailable []Unavailable
	var firstFailure error
	cacheHits := 0

	for _, p := range s.partners {
		quote, err := s.quoteFrom(ctx, tenant, p, request, forPartner)
		if err != nil {
			if !s.partial {
				return Response{}, err
			}
			if firstFailure == nil {
				firstFailure = err
			}
			unavailable = append(unavailable, Unavailable{Partner: p.Name, Reason: reasonOf(err)})
			continue
		}
		if quote.Source == SourceCache {
			cacheHits++
		}
		quotes = append(quotes, quote)
	}

	// No partner and no cache entry: there is nothing true to present, and a premium is never made up.
	if len(quotes) == 0 {
		s.metrics.response(ctx, tenant, outcomeUnavailable)
		return Response{}, firstFailure
	}

	sort.Slice(quotes, func(i, j int) bool {
		return quotes[i].PremiumCents < quotes[j].PremiumCents
	})

	degraded := len(unavailable) > 0
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Bool(AttrDegraded, degraded),
		attribute.Int(AttrCacheHits, cacheHits),
	)
	if degraded {
		s.metrics.response(ctx, tenant, outcomeDegraded)
	} else {
		s.metrics.response(ctx, tenant, outcomeComplete)
	}

	return Response{
		TenantID:    tenant,
		Quotes:      quotes,
		Degraded:    degraded,
		Unavailable: unavailable,
		ElapsedMs:   time.Since(start).Milliseconds(),
	}, nil
}

// quoteFrom gets one partner's quote: from the cache when this broker already has a live entry for
// this risk, from the partner otherwise.
func (s *Service) quoteFrom(ctx context.Context, tenant string, p platform.Partner, request Request, forPartner partnerRequest) (Quote, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, SpanPartnerQuote, trace.WithAttributes(
		attribute.String(AttrPartner, p.Name),
		attribute.String(AttrTenant, tenant),
	))
	defer span.End()

	key := CacheKey(tenant, p.Name, request)
	if cached, hit := s.lookup(ctx, span, tenant, p.Name, key); hit {
		return cached, nil
	}

	fetched, err := s.quoter.Quote(ctx, p, forPartner)
	if err != nil {
		return Quote{}, err
	}

	fetchedAt := s.now().UTC()
	s.store(ctx, key, fetched, fetchedAt)
	return Quote{Quote: fetched, Source: SourcePartner, FetchedAt: fetchedAt}, nil
}

func (s *Service) lookup(ctx context.Context, span trace.Span, tenant, partnerName, key string) (Quote, bool) {
	if s.cache == nil {
		return Quote{}, false
	}

	result := cacheMiss
	defer func() {
		span.SetAttributes(attribute.String(AttrCacheResult, result))
		s.metrics.cacheRequest(ctx, tenant, partnerName, result)
	}()

	value, found, err := s.cache.Get(ctx, key)
	if err != nil {
		result = cacheError
		return Quote{}, false
	}
	if !found {
		return Quote{}, false
	}

	var entry cacheEntry
	if err := json.Unmarshal(value, &entry); err != nil {
		result = cacheError
		return Quote{}, false
	}

	result = cacheHit
	return Quote{
		Quote:      entry.Quote,
		Source:     SourceCache,
		FetchedAt:  entry.FetchedAt,
		AgeSeconds: int64(s.now().Sub(entry.FetchedAt).Seconds()),
	}, true
}

// store never fails the request: a quote that could not be cached is simply bought again next time.
func (s *Service) store(ctx context.Context, key string, quote partner.Quote, fetchedAt time.Time) {
	if s.cache == nil {
		return
	}
	value, err := json.Marshal(cacheEntry{Quote: quote, FetchedAt: fetchedAt})
	if err != nil {
		return
	}
	_ = s.cache.Set(ctx, key, value, entryTTL(s.cacheTTL, quote))
}

func reasonOf(err error) string {
	var failure *partner.Error
	if errors.As(err, &failure) {
		switch {
		case failure.ShortCircuited:
			return ReasonCircuitOpen
		case failure.Timeout:
			return ReasonTimeout
		}
	}
	return ReasonError
}
