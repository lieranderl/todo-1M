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

	"github.com/nats-io/nats.go"
	"github.com/todo-1m/project/internal/app/domainengine"
	"github.com/todo-1m/project/internal/platform/env"
	"github.com/todo-1m/project/internal/platform/natsutil"
)

func main() {
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	healthAddr := env.String("DOMAIN_ENGINE_HEALTH_ADDR", ":8082")
	shutdownTimeout := env.Duration("SHUTDOWN_TIMEOUT", 10*time.Second)

	client, err := natsutil.ConnectJetStreamWithRetry(env.String("NATS_URL", env.DefaultNATSURL), env.Duration("NATS_CONNECT_TIMEOUT", 90*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	publisher := natsutil.JetStreamPublisher{JS: client.JS}
	service := domainengine.NewService(publisher.Publish)
	ready := atomic.Bool{}

	sub, err := client.JS.QueueSubscribe("app.command.>", "domain-engine", func(msg *nats.Msg) {
		if err := service.Handle(msg.Subject, msg.Data); err != nil {
			if errors.Is(err, domainengine.ErrInvalidCommandPayload) {
				log.Printf("discarding invalid command payload: %v", err)
				_ = msg.Term()
				return
			}
			if errors.Is(err, domainengine.ErrUnsupportedCommandAction) {
				log.Printf("discarding unsupported command action: %v", err)
				_ = msg.Term()
				return
			}
			log.Printf("command processing failed: %v", err)
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	}, nats.ManualAck())
	if err != nil {
		log.Fatal(err)
	}
	ready.Store(true)

	log.Println("Domain Engine listening on subject:", sub.Subject)
	log.Printf("Domain Engine health endpoint listening on %s", healthAddr)

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if client.Conn == nil || client.Conn.Status() != nats.CONNECTED {
			http.Error(w, "nats not connected", http.StatusServiceUnavailable)
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
		log.Printf("domain-engine subscription drain failed: %v", err)
	}
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("domain-engine health server shutdown failed: %v", err)
	}
}
