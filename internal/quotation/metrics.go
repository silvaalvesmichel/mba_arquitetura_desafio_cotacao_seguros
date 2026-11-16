package quotation

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	MetricCacheRequests = "quotation.cache.requests"
	MetricResponses     = "quotation.responses"
)

const (
	cacheHit   = "hit"
	cacheMiss  = "miss"
	cacheError = "error"

	outcomeComplete    = "complete"
	outcomeDegraded    = "degraded"
	outcomeUnavailable = "unavailable"
)

type metrics struct {
	cacheRequests metric.Int64Counter
	responses     metric.Int64Counter
}

func newMetrics() *metrics {
	meter := otel.Meter(tracerName)

	cacheRequests, err := meter.Int64Counter(MetricCacheRequests,
		metric.WithDescription("Cache lookups by result (hit, miss, error), partner and broker"))
	if err != nil {
		otel.Handle(err)
	}

	responses, err := meter.Int64Counter(MetricResponses,
		metric.WithDescription("Quotations answered by outcome (complete, degraded, unavailable) and broker"))
	if err != nil {
		otel.Handle(err)
	}

	return &metrics{cacheRequests: cacheRequests, responses: responses}
}

func (m *metrics) cacheRequest(ctx context.Context, tenant, partner, result string) {
	m.cacheRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result),
		attribute.String("partner", partner),
		attribute.String("tenant_id", tenant),
	))
}

func (m *metrics) response(ctx context.Context, tenant, outcome string) {
	m.responses.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("tenant_id", tenant),
	))
}
