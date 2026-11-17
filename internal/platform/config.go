package platform

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port       string
	Partners   []Partner
	Tenants    []string
	Telemetry  Telemetry
	Resilience Resilience
	Cache      Cache
}

// Resilience holds the parameters of the partner boundary. Every default is defended in docs/sad.md,
// section 4.
type Resilience struct {
	// PartnerTimeout caps the wait for one partner. A timeout counts as a failure for the breaker.
	PartnerTimeout time.Duration
	// BreakerFailureThreshold is the number of consecutive failures that opens a partner's circuit.
	BreakerFailureThreshold int
	// BreakerOpenFor is how long the circuit stays open before a single probe is let through.
	BreakerOpenFor time.Duration
}

type Cache struct {
	RedisAddr string
	// TTL of a cached quote. Never above the 24 hours insurers honor a quoted premium for.
	TTL time.Duration
	// Timeout of one Redis command. Past it the lookup counts as a miss.
	Timeout time.Duration
}

type Partner struct {
	Name    string
	BaseURL string
}

type Telemetry struct {
	ServiceName string
	Endpoint    string
	Enabled     bool
}

const defaultEndpoints = "partner-slow=http://localhost:9001," +
	"partner-flaky=http://localhost:9002," +
	"partner-degrading=http://localhost:9003"

const defaultTenants = "corretora-a,corretora-b"

const defaultCollector = "http://localhost:4317"

const defaultServiceName = "quotation-api"

const (
	defaultPartnerTimeoutMs        = 2000
	defaultBreakerFailureThreshold = 5
	defaultBreakerOpenSeconds      = 10
	defaultRedisAddr               = "localhost:6379"
	defaultCacheTTLSeconds         = 300
	defaultCacheTimeoutMs          = 100
)

// MaxCacheTTLSeconds is the commercial ceiling of 24 hours.
const MaxCacheTTLSeconds = 24 * 60 * 60

func LoadConfig(env func(string) string) (Config, error) {
	cfg := Config{Port: text(env, "PORT", "8080")}

	var err error
	if cfg.Partners, err = parsePartners(text(env, "PARTNER_ENDPOINTS", defaultEndpoints)); err != nil {
		return Config{}, err
	}
	if cfg.Tenants, err = parseTenants(text(env, "TENANTS", defaultTenants)); err != nil {
		return Config{}, err
	}
	if cfg.Telemetry, err = parseTelemetry(env); err != nil {
		return Config{}, err
	}
	if cfg.Resilience, err = parseResilience(env); err != nil {
		return Config{}, err
	}
	if cfg.Cache, err = parseCache(env); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseResilience(env func(string) string) (Resilience, error) {
	timeoutMs, err := positive(env, "PARTNER_TIMEOUT_MS", defaultPartnerTimeoutMs)
	if err != nil {
		return Resilience{}, err
	}
	threshold, err := positive(env, "BREAKER_FAILURE_THRESHOLD", defaultBreakerFailureThreshold)
	if err != nil {
		return Resilience{}, err
	}
	openSeconds, err := positive(env, "BREAKER_OPEN_SECONDS", defaultBreakerOpenSeconds)
	if err != nil {
		return Resilience{}, err
	}
	return Resilience{
		PartnerTimeout:          time.Duration(timeoutMs) * time.Millisecond,
		BreakerFailureThreshold: threshold,
		BreakerOpenFor:          time.Duration(openSeconds) * time.Second,
	}, nil
}

func parseCache(env func(string) string) (Cache, error) {
	ttlSeconds, err := positive(env, "CACHE_TTL_SECONDS", defaultCacheTTLSeconds)
	if err != nil {
		return Cache{}, err
	}
	if ttlSeconds > MaxCacheTTLSeconds {
		return Cache{}, fmt.Errorf("CACHE_TTL_SECONDS: %d is above the 24-hour ceiling (%d)", ttlSeconds, MaxCacheTTLSeconds)
	}
	timeoutMs, err := positive(env, "CACHE_TIMEOUT_MS", defaultCacheTimeoutMs)
	if err != nil {
		return Cache{}, err
	}
	return Cache{
		RedisAddr: strings.TrimSpace(text(env, "REDIS_ADDR", defaultRedisAddr)),
		TTL:       time.Duration(ttlSeconds) * time.Second,
		Timeout:   time.Duration(timeoutMs) * time.Millisecond,
	}, nil
}

func positive(env func(string) string, key string, fallback int) (int, error) {
	raw := strings.TrimSpace(env(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive integer", key, raw)
	}
	return value, nil
}

func parseTelemetry(env func(string) string) (Telemetry, error) {
	telemetry := Telemetry{
		ServiceName: text(env, "OTEL_SERVICE_NAME", defaultServiceName),
		Endpoint:    strings.TrimSpace(text(env, "OTEL_EXPORTER_OTLP_ENDPOINT", defaultCollector)),
	}

	switch disabled := strings.ToLower(strings.TrimSpace(env("OTEL_SDK_DISABLED"))); disabled {
	case "", "false":
		telemetry.Enabled = true
	case "true":
		return Telemetry{Enabled: false}, nil
	default:
		return Telemetry{}, fmt.Errorf("OTEL_SDK_DISABLED: %q is neither true nor false", disabled)
	}

	if u, err := url.Parse(telemetry.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return Telemetry{}, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT: %q is not an absolute URL", telemetry.Endpoint)
	}
	return telemetry, nil
}

func parsePartners(raw string) ([]Partner, error) {
	var partners []Partner
	seen := map[string]bool{}

	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		name, endpoint, found := strings.Cut(item, "=")
		name, endpoint = strings.TrimSpace(name), strings.TrimSpace(endpoint)
		if !found || name == "" || endpoint == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not in the name=url format", item)
		}

		if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not an absolute URL", endpoint)
		}
		if seen[name] {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: partner %q is repeated", name)
		}

		seen[name] = true
		partners = append(partners, Partner{Name: name, BaseURL: strings.TrimRight(endpoint, "/")})
	}

	if len(partners) == 0 {
		return nil, fmt.Errorf("PARTNER_ENDPOINTS: at least one partner is required")
	}
	return partners, nil
}

func parseTenants(raw string) ([]string, error) {
	var tenants []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tenants = append(tenants, t)
		}
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf("TENANTS: at least one broker is required")
	}
	return tenants, nil
}

func Environment() func(string) string { return os.Getenv }

func text(env func(string) string, key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}
