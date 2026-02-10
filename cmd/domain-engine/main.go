package main

import (
	"errors"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/todo-1m/project/internal/app/domainengine"
	"github.com/todo-1m/project/internal/platform/env"
	"github.com/todo-1m/project/internal/platform/natsutil"
)

func main() {
	client, err := natsutil.ConnectJetStreamWithRetry(env.String("NATS_URL", env.DefaultNATSURL), 20*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	publisher := natsutil.JetStreamPublisher{JS: client.JS}
	service := domainengine.NewService(publisher.Publish)

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

	log.Println("Domain Engine listening on subject:", sub.Subject)

	// Keep alive
	select {}
}
