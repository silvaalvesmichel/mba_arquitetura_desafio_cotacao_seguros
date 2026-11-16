package partner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/breaker"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

// The first 60 answers of partner-flaky with the default seed, as measured in
// docs/smoke-test-factibilidade.md (section 2.2): X is a 503, a dot is a 200. It holds a run of five
// failures at sequences 38-42 and the burst of nine at sequences 49-57.
const flakySequence = "...XX.XXX..X..X.X...XXX.....X..X.....XXXXX......XX" + "XXXXXXX.X."

// flakyPartner answers like partner-flaky does, by sequence number, and counts the calls that
// actually reached it.
func flakyPartner(t *testing.T) (platform.Partner, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seq := calls.Add(1)
		if flakySequence[seq-1] == 'X' {
			http.Error(w, `{"error":"partner unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintf(w, `{"quote_id":"q-%d","premium_cents":90000,"currency":"BRL","valid_for_seconds":300}`, seq)
	}))
	t.Cleanup(server.Close)

	return platform.Partner{Name: "partner-flaky", BaseURL: server.URL}, &calls
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func protectedClient(c *clock, transitions *[]string, options ...Option) *Client {
	settings := breaker.Settings{
		FailureThreshold: 5,
		OpenFor:          10 * time.Second,
		Now:              c.Now,
		OnTransition: func(from, to breaker.State) {
			*transitions = append(*transitions, from.String()+"->"+to.String())
		},
	}
	return NewClient(append([]Option{WithBreaker(settings)}, options...)...)
}

func shortCircuited(err error) bool {
	var failure *Error
	return errors.As(err, &failure) && failure.ShortCircuited
}

func TestBreakerOpensOnTheFlakyBurstAndStopsCallingThePartner(t *testing.T) {
	p, calls := flakyPartner(t)
	now := &clock{now: time.Unix(0, 0)}
	var transitions []string
	client := protectedClient(now, &transitions)
	ctx := context.Background()

	// Sequences 1 to 42: the fifth consecutive failure is sequence 42, and that is where it opens.
	for i := 1; i <= 42; i++ {
		_, err := client.Quote(ctx, p, map[string]string{})
		if shortCircuited(err) {
			t.Fatalf("call %d was short-circuited, the circuit should only open after sequence 42", i)
		}
	}

	// Open: the request is answered without reaching the partner.
	for i := 0; i < 3; i++ {
		if _, err := client.Quote(ctx, p, map[string]string{}); !shortCircuited(err) {
			t.Fatalf("call with the circuit open returned %v, expected a short-circuit", err)
		}
	}
	if got := calls.Load(); got != 42 {
		t.Fatalf("the partner received %d calls, expected 42: an open circuit must not call it", got)
	}

	// Half-open after 10 s: one probe (sequence 43, a success) closes the circuit.
	now.now = now.now.Add(10 * time.Second)
	if _, err := client.Quote(ctx, p, map[string]string{}); err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	// Closed again. Sequences 44-48 succeed, 49-53 are the first five of the burst: it reopens at 53.
	for i := 44; i <= 53; i++ {
		_, _ = client.Quote(ctx, p, map[string]string{})
	}
	if _, err := client.Quote(ctx, p, map[string]string{}); !shortCircuited(err) {
		t.Fatalf("after five failures of the burst the call returned %v, expected a short-circuit", err)
	}

	// Sequences 54-57 are still failures: each probe reopens. Sequence 58 succeeds and closes.
	for seq := 54; seq <= 58; seq++ {
		now.now = now.now.Add(10 * time.Second)
		_, _ = client.Quote(ctx, p, map[string]string{})
	}
	if got := calls.Load(); got != 58 {
		t.Fatalf("the partner received %d calls, expected 58", got)
	}

	expected := []string{
		"closed->open", "open->half_open", "half_open->closed",
		"closed->open",
		"open->half_open", "half_open->open",
		"open->half_open", "half_open->open",
		"open->half_open", "half_open->open",
		"open->half_open", "half_open->open",
		"open->half_open", "half_open->closed",
	}
	if fmt.Sprint(transitions) != fmt.Sprint(expected) {
		t.Fatalf("transitions\n got %v\nwant %v", transitions, expected)
	}
}

func TestTimeoutCountsAsFailureForTheBreaker(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// A partner that never answers: it sinks instead of failing.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	p := platform.Partner{Name: "partner-degrading", BaseURL: server.URL}

	now := &clock{now: time.Unix(0, 0)}
	var transitions []string
	client := protectedClient(now, &transitions, WithTimeout(50*time.Millisecond))

	for i := 1; i <= 5; i++ {
		_, err := client.Quote(context.Background(), p, map[string]string{})
		var failure *Error
		if !errors.As(err, &failure) || !failure.Timeout {
			t.Fatalf("call %d returned %v, expected a timeout", i, err)
		}
	}

	if _, err := client.Quote(context.Background(), p, map[string]string{}); !shortCircuited(err) {
		t.Fatalf("after five timeouts the call returned %v, expected a short-circuit", err)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("the partner received %d calls, expected 5", got)
	}
}

func TestCallerThatGivesUpDoesNotCountAgainstThePartner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	p := platform.Partner{Name: "partner-slow", BaseURL: server.URL}

	var transitions []string
	client := protectedClient(&clock{now: time.Unix(0, 0)}, &transitions)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 10; i++ {
		_, _ = client.Quote(ctx, p, map[string]string{})
	}
	if len(transitions) != 0 {
		t.Fatalf("transitions %v: brokers hanging up opened the partner's circuit", transitions)
	}
}

func TestShortCircuitIsMarkedOnTheSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	p, _ := flakyPartner(t)
	client := NewClient(WithBreaker(breaker.Settings{FailureThreshold: 1, OpenFor: time.Minute}))

	// Sequence 4 is the first failure: with a threshold of one, the fifth call is short-circuited.
	for i := 0; i < 4; i++ {
		_, _ = client.Quote(context.Background(), p, map[string]string{})
	}
	ctx, span := provider.Tracer("test").Start(context.Background(), "partner.quote")
	_, err := client.Quote(ctx, p, map[string]string{})
	span.End()
	if !shortCircuited(err) {
		t.Fatalf("expected a short-circuit, got %v", err)
	}

	marked := false
	for _, attr := range recorder.Ended()[0].Attributes() {
		if string(attr.Key) == AttrShortCircuited && attr.Value.AsBool() {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("the short-circuited request does not carry %s=true on its span", AttrShortCircuited)
	}
}
