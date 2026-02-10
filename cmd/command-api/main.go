package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/todo-1m/project/internal/app/commandapi"
	"github.com/todo-1m/project/internal/app/identity"
	"github.com/todo-1m/project/internal/app/query"
	"github.com/todo-1m/project/internal/platform/env"
	"github.com/todo-1m/project/internal/platform/natsutil"
)

func main() {
	ctx := context.Background()
	commandAddr := env.String("COMMAND_API_ADDR", env.DefaultCommandAddr)
	uiOrigin := env.String("UI_ORIGIN", "http://localhost:8081")
	pgURL := env.String("DATABASE_URL", env.DefaultDatabaseURL)
	jwtSecret := env.String("JWT_SECRET", "dev-insecure-change-me")

	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	identityRepo := identity.NewPostgresRepository(pool)
	if err := waitForIdentitySchema(ctx, identityRepo, 30*time.Second); err != nil {
		log.Fatal(err)
	}
	identitySvc := identity.NewService(identityRepo, identity.NewTokenManager(jwtSecret))
	todoQuery := query.NewTodoRepository(pool)

	client, err := natsutil.ConnectJetStreamWithRetry(env.String("NATS_URL", env.DefaultNATSURL), 20*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	publisher := natsutil.JetStreamPublisher{JS: client.JS}
	service := commandapi.NewService(publisher.Publish)
	handler := commandapi.NewHandler(service, identitySvc, todoQuery, uiOrigin)

	fmt.Printf("Command API listening on %s\n", commandAddr)
	log.Fatal(http.ListenAndServe(commandAddr, handler.Router()))
}

func waitForIdentitySchema(ctx context.Context, repo *identity.PostgresRepository, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = repo.EnsureSchema(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		log.Printf("waiting for identity schema readiness: %v", lastErr)
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}
