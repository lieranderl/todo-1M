package domainengine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/todo-1m/project/internal/contracts"
	"github.com/todo-1m/project/internal/sharding"
)

func TestHandle_PublishesCreatedEvent(t *testing.T) {
	var gotSubject string
	var gotPayload []byte

	svc := NewService(func(subject string, payload []byte) error {
		gotSubject = subject
		gotPayload = payload
		return nil
	})
	svc.NewID = func() string { return "evt-1" }
	svc.Now = func() time.Time { return time.Date(2026, 2, 9, 22, 0, 0, 0, time.UTC) }

	cmd := contracts.TodoCommand{
		CommandID:   "cmd-1",
		TodoID:      "todo-1",
		ActorUserID: "user-1",
		ActorName:   "alice",
		GroupID:     "group-1",
		Action:      "create-todo",
		Title:       "Buy Milk",
	}
	payload, _ := json.Marshal(cmd)

	if err := svc.Handle("app.command.532.group.group-1", payload); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	if gotSubject != "app.event.532.group.group-1" {
		t.Fatalf("unexpected event subject: %q", gotSubject)
	}
	var event contracts.TodoEvent
	if err := json.Unmarshal(gotPayload, &event); err != nil {
		t.Fatalf("event payload invalid JSON: %v", err)
	}
	if event.EventID != "evt-1" || event.CommandID != "cmd-1" || event.TodoID != "todo-1" || event.GroupID != "group-1" || event.ActorName != "alice" || event.EventType != "todo.created" || event.Title != "Buy Milk" || event.ShardID != 532 {
		t.Fatalf("unexpected event payload: %+v", event)
	}
}

func TestHandle_InvalidPayload(t *testing.T) {
	svc := NewService(func(_ string, _ []byte) error { return nil })
	err := svc.Handle("app.command.1.group.group-1", []byte("{invalid json"))
	if !errors.Is(err, ErrInvalidCommandPayload) {
		t.Fatalf("expected ErrInvalidCommandPayload, got %v", err)
	}
}

func TestHandle_UnsupportedAction(t *testing.T) {
	svc := NewService(func(_ string, _ []byte) error { return nil })
	cmd := contracts.TodoCommand{CommandID: "c1", TodoID: "t1", ActorUserID: "u1", GroupID: "g1", Action: "archive-todo"}
	payload, _ := json.Marshal(cmd)
	err := svc.Handle("app.command.1.group.group-1", payload)
	if !errors.Is(err, ErrUnsupportedCommandAction) {
		t.Fatalf("expected ErrUnsupportedCommandAction, got %v", err)
	}
}

func TestShardFromSubject_Fallback(t *testing.T) {
	got := ShardFromSubject("group-2", "bad.subject")
	want := sharding.GetShardID("group-2")
	if got != want {
		t.Fatalf("expected fallback shard %d, got %d", want, got)
	}
}
