package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/breaker"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation"
)

func main() {
	cfg, err := platform.LoadConfig(platform.Environment())
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	ctx, stopListening := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopListening()

	stopTelemetry, err := platform.StartTelemetry(context.Background(), cfg.Telemetry)
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}

	// The cache is an optimization, never a dependency: with Redis away every lookup is a miss and the
	// quotation is bought from the partners again.
	redisCache := cache.NewRedis(cfg.Cache.RedisAddr, cfg.Cache.Timeout)
	defer func() { _ = redisCache.Close() }()
	if err := redisCache.Ping(context.Background()); err != nil {
		log.Printf("quotation-api: redis at %s is not answering, starting without cache hits: %v", cfg.Cache.RedisAddr, err)
	}

	partners := partner.NewClient(
		partner.WithTimeout(cfg.Resilience.PartnerTimeout),
		partner.WithBreaker(breaker.Settings{
			FailureThreshold: cfg.Resilience.BreakerFailureThreshold,
			OpenFor:          cfg.Resilience.BreakerOpenFor,
		}),
	)

	api := quotation.NewAPI(
		quotation.NewService(cfg.Partners, partners,
			quotation.WithCache(redisCache, cfg.Cache.TTL),
			quotation.WithPartialResponses(),
		),
		cfg.Tenants,
	)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           platform.InstrumentHandler(api.Routes(), cfg.Telemetry.ServiceName),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Print("quotation-api: signal received, shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("quotation-api: forced shutdown: %v", err)
		}
		if err := stopTelemetry(shutdown); err != nil {
			log.Printf("quotation-api: telemetry shutdown: %v", err)
		}
	}()

	log.Printf("quotation-api listening on %s | partners=%d brokers=%v",
		server.Addr, len(cfg.Partners), cfg.Tenants)
	log.Printf("quotation-api: partner timeout=%s | breaker opens after %d consecutive failures, stays open %s | cache ttl=%s redis=%s",
		cfg.Resilience.PartnerTimeout, cfg.Resilience.BreakerFailureThreshold, cfg.Resilience.BreakerOpenFor,
		cfg.Cache.TTL, cfg.Cache.RedisAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}
