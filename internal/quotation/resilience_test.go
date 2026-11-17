package quotation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// fakeCache expires entries by the test clock, the way Redis does by its own.
type fakeCache struct {
	clock   *testClock
	entries map[string]fakeEntry
	err     error
}

type fakeEntry struct {
	value   []byte
	expires time.Time
}

func newFakeCache(clock *testClock) *fakeCache {
	return &fakeCache{clock: clock, entries: map[string]fakeEntry{}}
}

func (f *fakeCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	entry, found := f.entries[key]
	if !found || !f.clock.Now().Before(entry.expires) {
		return nil, false, nil
	}
	return entry.value, true, nil
}

func (f *fakeCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.entries[key] = fakeEntry{value: value, expires: f.clock.Now().Add(ttl)}
	return nil
}

// scriptedQuoter answers every partner with a quote that has an id and a declared validity, except
// the partners listed in down.
type scriptedQuoter struct {
	down     map[string]*partner.Error
	validFor int64
	calls    []string
}

func (q *scriptedQuoter) Quote(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	q.calls = append(q.calls, p.Name)
	if failure, isDown := q.down[p.Name]; isDown {
		return partner.Quote{}, failure
	}
	return partner.Quote{
		Partner:         p.Name,
		QuoteID:         p.Name + "-quote-id",
		PremiumCents:    defaultPremiums()[p.Name],
		Currency:        "BRL",
		ValidForSeconds: q.validFor,
	}, nil
}

const cacheTTL = 300 * time.Second

func cachedService(quoter Quoter, clock *testClock, cache Cache) *Service {
	return NewService(threePartners, quoter, WithCache(cache, cacheTTL), WithPartialResponses(), WithClock(clock.Now))
}

func TestRepeatedQuoteIsServedFromCacheWithoutCallingThePartners(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	quoter := &scriptedQuoter{}
	service := cachedService(quoter, clock, newFakeCache(clock))

	first, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	for _, quote := range first.Quotes {
		if quote.Source != SourcePartner || quote.AgeSeconds != 0 {
			t.Fatalf("first quote from %s has source %q and age %d, expected a fresh partner quote", quote.Partner, quote.Source, quote.AgeSeconds)
		}
	}

	clock.advance(42 * time.Second)
	second, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if len(quoter.calls) != 3 {
		t.Fatalf("the partners were called %d times, expected 3: the repeated quote was bought again", len(quoter.calls))
	}
	if second.Degraded || len(second.Quotes) != 3 {
		t.Fatalf("cached response is degraded=%v with %d quotes, expected a complete one", second.Degraded, len(second.Quotes))
	}
	for i, quote := range second.Quotes {
		if quote.Source != SourceCache || quote.AgeSeconds != 42 {
			t.Errorf("quote from %s has source %q and age %d, expected cache and 42", quote.Partner, quote.Source, quote.AgeSeconds)
		}
		// Traceable: same partner quote, same instant of the consultation that produced it.
		if quote.QuoteID != first.Quotes[i].QuoteID || !quote.FetchedAt.Equal(first.Quotes[i].FetchedAt) {
			t.Errorf("cached quote from %s does not trace back to the original consultation", quote.Partner)
		}
		if quote.PremiumCents != first.Quotes[i].PremiumCents {
			t.Errorf("cached premium %d differs from the partner's %d", quote.PremiumCents, first.Quotes[i].PremiumCents)
		}
	}
}

func TestCacheNeverCrossesBrokers(t *testing.T) {
	clock := &testClock{now: time.Unix(0, 0)}
	quoter := &scriptedQuoter{}
	cache := newFakeCache(clock)
	service := cachedService(quoter, clock, cache)

	if _, err := service.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}
	response, err := service.Quote(context.Background(), "corretora-b", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if len(quoter.calls) != 6 {
		t.Fatalf("the partners were called %d times, expected 6: broker B was served what broker A quoted", len(quoter.calls))
	}
	for _, quote := range response.Quotes {
		if quote.Source != SourcePartner {
			t.Fatalf("broker B got the quote of %s from %q, expected from the partner", quote.Partner, quote.Source)
		}
	}
	for key := range cache.entries {
		if !strings.HasPrefix(key, "quote:v1:corretora-a:") && !strings.HasPrefix(key, "quote:v1:corretora-b:") {
			t.Fatalf("cache key %q does not start with the broker", key)
		}
	}
	if len(cache.entries) != 6 {
		t.Fatalf("%d cache entries, expected 6 (three partners for each of two brokers)", len(cache.entries))
	}
}

func TestCacheKeyIsWrittenOut(t *testing.T) {
	request := validRequest()
	key := CacheKey("corretora-a", "partner-slow", request)

	if !regexp.MustCompile(`^quote:v1:corretora-a:partner-slow:[0-9a-f]{64}$`).MatchString(key) {
		t.Fatalf("key %q is not quote:v1:{tenant_id}:{partner}:{sha256}", key)
	}
	if strings.Contains(key, request.Driver.Document) || strings.Contains(key, request.Vehicle.Plate) {
		t.Fatalf("key %q exposes the document or the plate", key)
	}

	different := map[string]string{
		"broker":  CacheKey("corretora-b", "partner-slow", request),
		"partner": CacheKey("corretora-a", "partner-flaky", request),
	}
	changes := map[string]func(*Request){
		"document":   func(r *Request) { r.Driver.Document = "98765432100" },
		"birth year": func(r *Request) { r.Driver.BirthYear = 1990 },
		"plate":      func(r *Request) { r.Vehicle.Plate = "XYZ9A87" },
		"model":      func(r *Request) { r.Vehicle.Model = "Onix 1.0" },
		"year":       func(r *Request) { r.Vehicle.Year = 2021 },
		"value":      func(r *Request) { r.Vehicle.ValueCents = 9000000 },
		"coverage":   func(r *Request) { r.Coverage = "third_party" },
	}
	for name, change := range changes {
		other := validRequest()
		change(&other)
		different[name] = CacheKey("corretora-a", "partner-slow", other)
	}
	for name, other := range different {
		if other == key {
			t.Errorf("changing the %s kept the same cache key", name)
		}
	}

	// A broker name can never be read as a different broker plus a partner.
	if CacheKey("a:partner-slow", "x", request) == CacheKey("a", "partner-slow:x", request) {
		t.Error("the separator inside a broker name produced a colliding key")
	}
}

func TestCacheEntryExpiresWithTheTTL(t *testing.T) {
	clock := &testClock{now: time.Unix(0, 0)}
	quoter := &scriptedQuoter{}
	service := cachedService(quoter, clock, newFakeCache(clock))

	_, _ = service.Quote(context.Background(), "corretora-a", validRequest())
	clock.advance(cacheTTL - time.Second)
	_, _ = service.Quote(context.Background(), "corretora-a", validRequest())
	if len(quoter.calls) != 3 {
		t.Fatalf("%d partner calls one second before the TTL, expected 3", len(quoter.calls))
	}

	clock.advance(time.Second)
	response, _ := service.Quote(context.Background(), "corretora-a", validRequest())
	if len(quoter.calls) != 6 {
		t.Fatalf("%d partner calls at the TTL, expected 6: an expired quote was served", len(quoter.calls))
	}
	if response.Quotes[0].Source != SourcePartner {
		t.Fatalf("quote at the TTL came from %q, expected from the partner", response.Quotes[0].Source)
	}
}

func TestCacheNeverOutlivesTheValidityThePartnerDeclared(t *testing.T) {
	clock := &testClock{now: time.Unix(0, 0)}
	quoter := &scriptedQuoter{validFor: 120}
	service := cachedService(quoter, clock, newFakeCache(clock))

	_, _ = service.Quote(context.Background(), "corretora-a", validRequest())
	clock.advance(120 * time.Second)
	_, _ = service.Quote(context.Background(), "corretora-a", validRequest())

	if len(quoter.calls) != 6 {
		t.Fatalf("%d partner calls, expected 6: a quote valid for 120 s was served from cache after 120 s", len(quoter.calls))
	}
}

func TestBrokenCacheIsAMissNotAFailure(t *testing.T) {
	clock := &testClock{now: time.Unix(0, 0)}
	quoter := &scriptedQuoter{}
	cache := newFakeCache(clock)
	cache.err = errors.New("redis: connection refused")

	response, err := cachedService(quoter, clock, cache).Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote with the cache down: %v", err)
	}
	if response.Degraded || len(response.Quotes) != 3 || len(quoter.calls) != 3 {
		t.Fatalf("with the cache down: degraded=%v quotes=%d partner calls=%d, expected a complete response bought from the partners",
			response.Degraded, len(response.Quotes), len(quoter.calls))
	}
}

func TestPartnerDownDegradesTheResponseInsteadOfFailingIt(t *testing.T) {
	clock := &testClock{now: time.Unix(0, 0)}
	quoter := &scriptedQuoter{down: map[string]*partner.Error{
		"partner-flaky": {Partner: "partner-flaky", Status: 503, Reason: "partner replied 503"},
	}}

	response, err := cachedService(quoter, clock, newFakeCache(clock)).Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("one partner down failed the whole request: %v", err)
	}

	if !response.Degraded {
		t.Fatal("a response missing a partner is not flagged as degraded")
	}
	if len(response.Unavailable) != 1 || response.Unavailable[0] != (Unavailable{Partner: "partner-flaky", Reason: ReasonError}) {
		t.Fatalf("unavailable %+v, expected partner-flaky with reason error", response.Unavailable)
	}
	if len(response.Quotes) != 2 || response.Quotes[0].Partner != "partner-degrading" || response.Quotes[1].Partner != "partner-slow" {
		t.Fatalf("quotes %+v, expected partner-degrading then partner-slow (sorted by premium)", response.Quotes)
	}
	if last := quoter.calls[len(quoter.calls)-1]; last != "partner-degrading" {
		t.Fatalf("last partner called was %q: the partner after the failed one was not consulted", last)
	}
}

func TestDegradedResponseSaysWhyEachPartnerIsMissing(t *testing.T) {
	clock := &testClock{now: time.Unix(0, 0)}
	quoter := &scriptedQuoter{down: map[string]*partner.Error{
		"partner-flaky":     {Partner: "partner-flaky", Reason: "circuit open", ShortCircuited: true},
		"partner-degrading": {Partner: "partner-degrading", Reason: "deadline exceeded", Timeout: true},
	}}

	response, err := cachedService(quoter, clock, newFakeCache(clock)).Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	expected := []Unavailable{
		{Partner: "partner-flaky", Reason: ReasonCircuitOpen},
		{Partner: "partner-degrading", Reason: ReasonTimeout},
	}
	if len(response.Unavailable) != 2 || response.Unavailable[0] != expected[0] || response.Unavailable[1] != expected[1] {
		t.Fatalf("unavailable %+v, expected %+v", response.Unavailable, expected)
	}
}

func TestFailedPartnerIsNotCached(t *testing.T) {
	clock := &testClock{now: time.Unix(0, 0)}
	quoter := &scriptedQuoter{down: map[string]*partner.Error{
		"partner-flaky": {Partner: "partner-flaky", Status: 503, Reason: "partner replied 503"},
	}}
	service := cachedService(quoter, clock, newFakeCache(clock))

	_, _ = service.Quote(context.Background(), "corretora-a", validRequest())
	quoter.down = nil
	response, _ := service.Quote(context.Background(), "corretora-a", validRequest())

	if response.Degraded || len(response.Quotes) != 3 {
		t.Fatalf("after the partner came back: degraded=%v quotes=%d, expected a complete response", response.Degraded, len(response.Quotes))
	}
	// Four calls: three on the first request, and only the partner that had failed on the second.
	if len(quoter.calls) != 4 || quoter.calls[3] != "partner-flaky" {
		t.Fatalf("partner calls %v, expected the second request to consult only partner-flaky", quoter.calls)
	}
}

func degradedAPI(quoter Quoter) http.Handler {
	clock := &testClock{now: time.Unix(0, 0)}
	return NewAPI(cachedService(quoter, clock, newFakeCache(clock)), []string{"corretora-a", "corretora-b"}).Routes()
}

func TestQuotesAnswers200FlaggedAsDegradedWhenAPartnerIsDown(t *testing.T) {
	quoter := &scriptedQuoter{down: map[string]*partner.Error{
		"partner-flaky": {Partner: "partner-flaky", Reason: "circuit open", ShortCircuited: true},
	}}
	response := postQuotes(degradedAPI(quoter), "corretora-a", validBody)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d, expected 200: %s", response.Code, response.Body)
	}

	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response: %v", err)
	}
	if body["degraded"] != true {
		t.Fatalf("the broker cannot tell the response is degraded: %s", response.Body)
	}
	missing := body["unavailable"].([]any)[0].(map[string]any)
	if missing["partner"] != "partner-flaky" || missing["reason"] != ReasonCircuitOpen {
		t.Fatalf("unavailable %v, expected partner-flaky with reason circuit_open", missing)
	}
	quote := body["quotes"].([]any)[0].(map[string]any)
	for _, field := range []string{"source", "fetched_at", "age_seconds", "quote_id", "premium_cents"} {
		if _, present := quote[field]; !present {
			t.Errorf("quote in the response has no %q: %v", field, quote)
		}
	}
}

func TestCompleteResponseSaysItIsNotDegraded(t *testing.T) {
	response := postQuotes(degradedAPI(&scriptedQuoter{}), "corretora-a", validBody)

	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response: %v", err)
	}
	if degraded, present := body["degraded"]; !present || degraded != false {
		t.Fatalf("complete response does not say degraded=false: %s", response.Body)
	}
	if _, present := body["unavailable"]; present {
		t.Fatalf("complete response lists unavailable partners: %s", response.Body)
	}
}

func TestQuotesStillAnswers502WhenNoPartnerCanQuote(t *testing.T) {
	quoter := &scriptedQuoter{down: map[string]*partner.Error{
		"partner-slow":      {Partner: "partner-slow", Reason: "deadline exceeded", Timeout: true},
		"partner-flaky":     {Partner: "partner-flaky", Status: 503, Reason: "partner replied 503"},
		"partner-degrading": {Partner: "partner-degrading", Reason: "circuit open", ShortCircuited: true},
	}}
	response := postQuotes(degradedAPI(quoter), "corretora-a", validBody)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status %d, expected 502: with no quote at all there is nothing true to present", response.Code)
	}
	var body platform.ErrorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable error: %v", err)
	}
	if body.Message != "partner insurer unavailable" || body.Partner != "partner-slow" {
		t.Fatalf("error body %+v, expected the existing partner-unavailable error", body)
	}
}

func TestSpansMarkCacheHitsAndCarryNoPersonalData(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	clock := &testClock{now: time.Unix(0, 0)}
	service := cachedService(&scriptedQuoter{}, clock, newFakeCache(clock))
	request := validRequest()
	_, _ = service.Quote(context.Background(), "corretora-a", request)
	_, _ = service.Quote(context.Background(), "corretora-a", request)

	spans := recorder.Ended()
	if len(spans) != 6 {
		t.Fatalf("%d spans, expected 6 (one per partner, two requests)", len(spans))
	}

	results := make([]string, 0, len(spans))
	for _, span := range spans {
		if span.Name() != SpanPartnerQuote {
			t.Fatalf("span %q, expected %q", span.Name(), SpanPartnerQuote)
		}
		for _, attr := range span.Attributes() {
			value := attr.Value.Emit()
			if string(attr.Key) == AttrCacheResult {
				results = append(results, value)
			}
			for _, personal := range []string{request.Driver.Document, request.Vehicle.Plate, "-quote-id"} {
				if strings.Contains(value, personal) || strings.Contains(string(attr.Key), "quote_id") {
					t.Errorf("span attribute %s=%s carries personal data or a quote_id", attr.Key, value)
				}
			}
		}
	}
	if strings.Join(results, ",") != "miss,miss,miss,hit,hit,hit" {
		t.Fatalf("cache results on the spans: %v, expected three misses then three hits", results)
	}
}
