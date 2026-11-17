package partner

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/breaker"
)

const (
	MetricBreakerState       = "quotation.breaker.state"
	MetricBreakerTransitions = "quotation.breaker.transitions"
	MetricPartnerDuration    = "quotation.partner.duration"
)

const meterName = "github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"

// Seconds. Dense around the partner profiles (120 ms, 150 ms, 1.5 s) and the 2 s client timeout.
var durationBuckets = []float64{0.05, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 5, 7.5, 10}

type metrics struct {
	transitions metric.Int64Counter
	duration    metric.Float64Histogram
}

func newMetrics(states func() map[string]breaker.State) *metrics {
	meter := otel.Meter(meterName)

	transitions, err := meter.Int64Counter(MetricBreakerTransitions,
		metric.WithDescription("Circuit breaker state changes, by partner, origin and destination"))
	if err != nil {
		otel.Handle(err)
	}

	duration, err := meter.Float64Histogram(MetricPartnerDuration,
		metric.WithDescription("Time waited for a partner insurer, for calls that actually went out"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	if err != nil {
		otel.Handle(err)
	}

	_, err = meter.Int64ObservableGauge(MetricBreakerState,
		metric.WithDescription("Circuit breaker state by partner: 0 closed, 1 half_open, 2 open"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			for name, state := range states() {
				observer.Observe(int64(state), metric.WithAttributes(attribute.String("partner", name)))
			}
			return nil
		}))
	if err != nil {
		otel.Handle(err)
	}

	return &metrics{transitions: transitions, duration: duration}
}

func (m *metrics) transition(partner string, from, to breaker.State) {
	m.transitions.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("partner", partner),
		attribute.String("from", from.String()),
		attribute.String("to", to.String()),
	))
}

func (m *metrics) observe(ctx context.Context, partner, outcome string, elapsed time.Duration) {
	m.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
		attribute.String("partner", partner),
		attribute.String("outcome", outcome),
	))
}
