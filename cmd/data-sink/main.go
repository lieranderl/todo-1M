package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/todo-1m/project/internal/app/datasink"
	"github.com/todo-1m/project/internal/platform/env"
	"github.com/todo-1m/project/internal/platform/natsutil"
)

func main() {
	ctx := context.Background()

	natsURL := env.String("NATS_URL", env.DefaultNATSURL)
	pgURL := env.String("DATABASE_URL", env.DefaultDatabaseURL)

	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	repository := datasink.NewEventRepository(pool)
	if err := waitForPostgres(ctx, pool, repository, 30*time.Second); err != nil {
		log.Fatal(err)
	}
	service := datasink.NewService(repository)

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

		insertCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
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

	log.Println("Data Sink listening on subject:", sub.Subject)

	// Keep alive
	select {}
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
