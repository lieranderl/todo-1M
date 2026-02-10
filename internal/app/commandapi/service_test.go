package commandapi

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/todo-1m/project/internal/contracts"
	"github.com/todo-1m/project/internal/sharding"
)

func TestAccept_CreatePublishesCommand(t *testing.T) {
	var gotSubject string
	var gotPayload []byte

	svc := NewService(func(subject string, payload []byte) error {
		gotSubject = subject
		gotPayload = payload
		return nil
	})
	svc.Now = func() time.Time { return time.Date(2026, 2, 9, 22, 0, 0, 0, time.UTC) }
	svc.NewID = func() string { return "cmd-1" }

	resp, err := svc.Accept(Actor{UserID: "user-1", Username: "alice"}, CreateTodoRequest{
		Action:  "create-todo",
		Title:   "Buy Milk",
		GroupID: "group-1",
	})
	if err != nil {
		t.Fatalf("Accept returned error: %v", err)
	}
	if resp.Status != "accepted" || resp.CommandID != "cmd-1" || resp.TodoID != "cmd-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	wantSubject := sharding.GetSubject("group", "group-1")
	if gotSubject != wantSubject {
		t.Fatalf("subject mismatch: got %q want %q", gotSubject, wantSubject)
	}

	var cmd contracts.TodoCommand
	if err := json.Unmarshal(gotPayload, &cmd); err != nil {
		t.Fatalf("payload is not valid TodoCommand JSON: %v", err)
	}
	if cmd.CommandID != "cmd-1" || cmd.TodoID != "cmd-1" || cmd.ActorUserID != "user-1" || cmd.ActorName != "alice" || cmd.GroupID != "group-1" || cmd.Title != "Buy Milk" {
		t.Fatalf("unexpected command payload: %+v", cmd)
	}
}

func TestAccept_UpdateRequiresTodoID(t *testing.T) {
	svc := NewService(func(_ string, _ []byte) error { return nil })
	_, err := svc.Accept(Actor{UserID: "user-1"}, CreateTodoRequest{Action: "update-todo", Title: "x", GroupID: "g1"})
	if !errors.Is(err, ErrTodoIDRequired) {
		t.Fatalf("expected ErrTodoIDRequired, got %v", err)
	}
}

func TestAccept_DeleteAllowsEmptyTitle(t *testing.T) {
	var got contracts.TodoCommand
	svc := NewService(func(_ string, payload []byte) error {
		return json.Unmarshal(payload, &got)
	})
	svc.NewID = func() string { return "cmd-delete" }

	resp, err := svc.Accept(Actor{UserID: "user-1", Username: "alice"}, CreateTodoRequest{Action: "delete-todo", GroupID: "g1", TodoID: "todo-1"})
	if err != nil {
		t.Fatalf("Accept returned error: %v", err)
	}
	if resp.TodoID != "todo-1" {
		t.Fatalf("unexpected todo id: %+v", resp)
	}
	if got.Title != "" || got.TodoID != "todo-1" {
		t.Fatalf("unexpected command: %+v", got)
	}
}

func TestAccept_UnsupportedAction(t *testing.T) {
	svc := NewService(func(_ string, _ []byte) error { return nil })
	_, err := svc.Accept(Actor{UserID: "u1"}, CreateTodoRequest{Action: "archive-todo", GroupID: "g1"})
	if !errors.Is(err, ErrUnsupportedAction) {
		t.Fatalf("expected ErrUnsupportedAction, got %v", err)
	}
}

func TestAccept_RequiresGroupID(t *testing.T) {
	svc := NewService(func(_ string, _ []byte) error { return nil })
	_, err := svc.Accept(Actor{UserID: "user-1"}, CreateTodoRequest{Title: "Buy Milk"})
	if !errors.Is(err, ErrGroupRequired) {
		t.Fatalf("expected ErrGroupRequired, got %v", err)
	}
}
