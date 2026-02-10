package commandapi

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/nats-io/nuid"
	"github.com/todo-1m/project/internal/contracts"
	"github.com/todo-1m/project/internal/sharding"
)

var ErrTitleRequired = errors.New("title is required")
var ErrGroupRequired = errors.New("group_id is required")
var ErrTodoIDRequired = errors.New("todo_id is required")
var ErrUnsupportedAction = errors.New("unsupported action")

type PublishFunc func(subject string, payload []byte) error

type Service struct {
	Publish PublishFunc
	Now     func() time.Time
	NewID   func() string
}

type Actor struct {
	UserID   string
	Username string
}

type CreateTodoRequest struct {
	Action  string `json:"action"`
	TodoID  string `json:"todo_id"`
	Title   string `json:"title"`
	GroupID string `json:"group_id"`
}

type CommandResponse struct {
	Status    string `json:"status"`
	CommandID string `json:"command_id"`
	TodoID    string `json:"todo_id"`
}

func NewService(publish PublishFunc) *Service {
	return &Service{
		Publish: publish,
		Now:     func() time.Time { return time.Now().UTC() },
		NewID:   nuid.Next,
	}
}

func normalizeAction(action string) string {
	action = strings.TrimSpace(strings.ToLower(action))
	if action == "" {
		return "create-todo"
	}
	return action
}

func (s *Service) Accept(actor Actor, req CreateTodoRequest) (CommandResponse, error) {
	groupID := strings.TrimSpace(req.GroupID)
	if groupID == "" {
		return CommandResponse{}, ErrGroupRequired
	}
	action := normalizeAction(req.Action)
	todoID := strings.TrimSpace(req.TodoID)
	title := strings.TrimSpace(req.Title)

	switch action {
	case "create-todo":
		if title == "" {
			return CommandResponse{}, ErrTitleRequired
		}
	case "update-todo":
		if todoID == "" {
			return CommandResponse{}, ErrTodoIDRequired
		}
		if title == "" {
			return CommandResponse{}, ErrTitleRequired
		}
	case "delete-todo":
		if todoID == "" {
			return CommandResponse{}, ErrTodoIDRequired
		}
	default:
		return CommandResponse{}, ErrUnsupportedAction
	}

	if strings.TrimSpace(actor.UserID) == "" {
		actor.UserID = "user-1"
	}
	if strings.TrimSpace(actor.Username) == "" {
		actor.Username = "unknown"
	}

	commandID := s.NewID()
	if todoID == "" {
		// For create, make todo ID stable and explicit for later update/delete.
		todoID = commandID
	}

	cmd := contracts.TodoCommand{
		CommandID:   commandID,
		TodoID:      todoID,
		ActorUserID: actor.UserID,
		ActorName:   actor.Username,
		GroupID:     groupID,
		Action:      action,
		Title:       title,
		CreatedAt:   s.Now(),
	}
	payload, err := json.Marshal(cmd)
	if err != nil {
		return CommandResponse{}, err
	}

	subject := sharding.GetSubject("group", groupID)
	if err := s.Publish(subject, payload); err != nil {
		return CommandResponse{}, err
	}

	return CommandResponse{
		Status:    "accepted",
		CommandID: cmd.CommandID,
		TodoID:    cmd.TodoID,
	}, nil
}
