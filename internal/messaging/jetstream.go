package messaging

import (
	"errors"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	commandsStream = "COMMANDS"
	eventsStream   = "EVENTS"
)

// EnsureStreams creates (or validates) the two streams required locally:
// - app.command.>
// - app.event.>
func EnsureStreams(js nats.JetStreamContext) error {
	if _, err := js.StreamInfo(commandsStream); err != nil {
		if errors.Is(err, nats.ErrStreamNotFound) {
			if _, addErr := js.AddStream(&nats.StreamConfig{
				Name:      commandsStream,
				Subjects:  []string{"app.command.>"},
				Retention: nats.LimitsPolicy,
				Storage:   nats.FileStorage,
				Replicas:  1,
				MaxAge:    24 * time.Hour,
				MaxBytes:  4 * 1024 * 1024 * 1024, // 4 GiB
				Discard:   nats.DiscardOld,
			}); addErr != nil {
				return addErr
			}
		} else {
			return err
		}
	}

	if _, err := js.StreamInfo(eventsStream); err != nil {
		if errors.Is(err, nats.ErrStreamNotFound) {
			if _, addErr := js.AddStream(&nats.StreamConfig{
				Name:      eventsStream,
				Subjects:  []string{"app.event.>"},
				Retention: nats.LimitsPolicy,
				Storage:   nats.FileStorage,
				Replicas:  1,
				MaxAge:    7 * 24 * time.Hour,
				MaxBytes:  32 * 1024 * 1024 * 1024, // 32 GiB
				Discard:   nats.DiscardOld,
			}); addErr != nil {
				return addErr
			}
		} else {
			return err
		}
	}

	return nil
}
