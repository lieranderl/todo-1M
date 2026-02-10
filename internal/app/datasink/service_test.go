package datasink

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/todo-1m/project/internal/contracts"
)

type fakeRepository struct {
	gotEvent contracts.TodoEvent
	gotSeq   uint64
	err      error
}

func (f *fakeRepository) InsertEvent(_ context.Context, event contracts.TodoEvent, eventSeq uint64) error {
	f.gotEvent = event
	f.gotSeq = eventSeq
	return f.err
}

func TestHandle_ValidEvent(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewService(repo)

	event := contracts.TodoEvent{
		EventID:     "evt-1",
		CommandID:   "cmd-1",
		TodoID:      "todo-1",
		GroupID:     "group-1",
		ActorUserID: "user-1",
		ActorName:   "alice",
		EventType:   "todo.created",
		Title:       "Buy Milk",
		ShardID:     532,
		OccurredAt:  time.Now().UTC(),
	}
	payload, _ := json.Marshal(event)

	if err := svc.Handle(context.Background(), payload, 42); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if repo.gotEvent.EventID != "evt-1" || repo.gotEvent.Title != "Buy Milk" || repo.gotEvent.TodoID != "todo-1" {
		t.Fatalf("unexpected event in repository: %+v", repo.gotEvent)
	}
	if repo.gotSeq != 42 {
		t.Fatalf("expected event sequence 42, got %d", repo.gotSeq)
	}
}

func TestHandle_InvalidPayload(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewService(repo)
	err := svc.Handle(context.Background(), []byte("{invalid"), 1)
	if !errors.Is(err, ErrInvalidEventPayload) {
		t.Fatalf("expected ErrInvalidEventPayload, got %v", err)
	}
}
