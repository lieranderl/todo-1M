package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/todo-1m/project/internal/app/datasink"
	"github.com/todo-1m/project/internal/platform/env"
	"github.com/todo-1m/project/internal/platform/natsutil"
)

func main() {
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	natsURL := env.String("NATS_URL", env.DefaultNATSURL)
	pgURL := env.String("DATABASE_URL", env.DefaultDatabaseURL)
	healthAddr := env.String("DATA_SINK_HEALTH_ADDR", ":8083")
	shutdownTimeout := parseDurationOrDefault(env.String("SHUTDOWN_TIMEOUT", "10s"), 10*time.Second)

	pool, err := pgxpool.New(runCtx, pgURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	repository := datasink.NewEventRepository(pool)
	if err := waitForPostgres(runCtx, pool, repository, 30*time.Second); err != nil {
		log.Fatal(err)
	}
	service := datasink.NewService(repository)
	ready := atomic.Bool{}

	client, err := natsutil.ConnectJetStreamWithRetry(natsURL, 20*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	sub, err := client.JS.QueueSubscribe("app.event.>", "data-sink", func(msg *nats.Msg) {
		var eventSeq uint64
		if meta, metaErr := msg.Metadata(); metaErr == nil {
			eventSeq = meta.Sequence.Stream
		}

		insertCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := service.Handle(insertCtx, msg.Data, eventSeq); err != nil {
			if errors.Is(err, datasink.ErrInvalidEventPayload) {
				log.Printf("discarding invalid event payload: %v", err)
				_ = msg.Term()
				return
			}
			if errors.Is(err, datasink.ErrUnsupportedEventType) {
				log.Printf("discarding unsupported event type: %v", err)
				_ = msg.Term()
				return
			}
			log.Printf("event persistence failed: %v", err)
			_ = msg.Nak()
			return
		}

		_ = msg.Ack()
	}, nats.ManualAck())
	if err != nil {
		log.Fatal(err)
	}
	ready.Store(true)

	log.Println("Data Sink listening on subject:", sub.Subject)
	log.Printf("Data Sink health endpoint listening on %s", healthAddr)

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := checkDataSinkReadiness(r.Context(), pool, client.Conn, &ready); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthServer := &http.Server{
		Addr:              healthAddr,
		Handler:           healthMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	healthErr := make(chan error, 1)
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			healthErr <- err
		}
	}()

	select {
	case err := <-healthErr:
		log.Fatal(err)
	case <-runCtx.Done():
	}

	ready.Store(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := sub.Drain(); err != nil {
		log.Printf("data-sink subscription drain failed: %v", err)
	}
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("data-sink health server shutdown failed: %v", err)
	}
}

func waitForPostgres(
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *datasink.EventRepository,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = pool.Ping(attemptCtx)
		if lastErr == nil {
			lastErr = repository.EnsureSchema(attemptCtx)
		}
		cancel()

		if lastErr == nil {
			return nil
		}
		log.Printf("waiting for postgres readiness: %v", lastErr)
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func checkDataSinkReadiness(ctx context.Context, pool *pgxpool.Pool, conn *nats.Conn, ready *atomic.Bool) error {
	if ready == nil || !ready.Load() {
		return errors.New("not ready")
	}
	if conn == nil {
		return errors.New("nats connection is nil")
	}
	if conn.Status() != nats.CONNECTED {
		return errors.New("nats not connected")
	}

	checkCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := pool.Ping(checkCtx); err != nil {
		return err
	}
	return nil
}

func parseDurationOrDefault(raw string, fallback time.Duration) time.Duration {
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
