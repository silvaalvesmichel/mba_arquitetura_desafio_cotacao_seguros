package platform

import (
	"testing"
	"time"
)

func TestResilienceDefaultsAreTheOnesDefendedInTheSAD(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := Resilience{PartnerTimeout: 2 * time.Second, BreakerFailureThreshold: 5, BreakerOpenFor: 10 * time.Second}
	if cfg.Resilience != want {
		t.Errorf("resilience %+v, want %+v", cfg.Resilience, want)
	}
	if cfg.Cache != (Cache{RedisAddr: "localhost:6379", TTL: 300 * time.Second, Timeout: 100 * time.Millisecond}) {
		t.Errorf("unexpected cache defaults %+v", cfg.Cache)
	}
}

func TestResilienceIsReadFromTheEnvironment(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"PARTNER_TIMEOUT_MS":        "1500",
		"BREAKER_FAILURE_THRESHOLD": "3",
		"BREAKER_OPEN_SECONDS":      "30",
		"REDIS_ADDR":                "redis:6379",
		"CACHE_TTL_SECONDS":         "86400",
		"CACHE_TIMEOUT_MS":          "50",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := Resilience{PartnerTimeout: 1500 * time.Millisecond, BreakerFailureThreshold: 3, BreakerOpenFor: 30 * time.Second}
	if cfg.Resilience != want {
		t.Errorf("resilience %+v, want %+v", cfg.Resilience, want)
	}
	if cfg.Cache != (Cache{RedisAddr: "redis:6379", TTL: 24 * time.Hour, Timeout: 50 * time.Millisecond}) {
		t.Errorf("unexpected cache %+v", cfg.Cache)
	}
}

func TestCacheTTLAboveTheCommercialCeilingIsRejected(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{"CACHE_TTL_SECONDS": "86401"})); err == nil {
		t.Fatal("a cache TTL above 24 hours was accepted")
	}
}

func TestInvalidResilienceValuesAreRejected(t *testing.T) {
	for _, key := range []string{"PARTNER_TIMEOUT_MS", "BREAKER_FAILURE_THRESHOLD", "BREAKER_OPEN_SECONDS", "CACHE_TTL_SECONDS", "CACHE_TIMEOUT_MS"} {
		for _, value := range []string{"0", "-1", "abc"} {
			if _, err := LoadConfig(env(map[string]string{key: value})); err == nil {
				t.Errorf("%s=%s was accepted", key, value)
			}
		}
	}
}
