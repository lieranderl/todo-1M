package domainengine

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nuid"
	"github.com/todo-1m/project/internal/contracts"
	"github.com/todo-1m/project/internal/sharding"
)

var ErrInvalidCommandPayload = errors.New("invalid command payload")

// ErrUnsupportedCommandAction prevents unknown write-model transitions.
var ErrUnsupportedCommandAction = errors.New("unsupported command action")

type PublishFunc func(subject string, payload []byte) error

type Service struct {
	Publish PublishFunc
	Now     func() time.Time
	NewID   func() string
}

func NewService(publish PublishFunc) *Service {
	return &Service{
		Publish: publish,
		Now:     func() time.Time { return time.Now().UTC() },
		NewID:   nuid.Next,
	}
}

func (s *Service) Handle(commandSubject string, commandPayload []byte) error {
	var cmd contracts.TodoCommand
	if err := json.Unmarshal(commandPayload, &cmd); err != nil {
		return ErrInvalidCommandPayload
	}

	shardID := ShardFromSubject(cmd.GroupID, commandSubject)
	eventType, err := mapEventType(cmd.Action)
	if err != nil {
		return err
	}

	event := contracts.TodoEvent{
		EventID:     s.NewID(),
		CommandID:   cmd.CommandID,
		TodoID:      cmd.TodoID,
		GroupID:     cmd.GroupID,
		ActorUserID: cmd.ActorUserID,
		ActorName:   cmd.ActorName,
		EventType:   eventType,
		Title:       cmd.Title,
		OccurredAt:  s.Now(),
		ShardID:     shardID,
	}

	eventPayload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.Publish(EventSubject(event), eventPayload)
}

func mapEventType(action string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(action)) {
	case "create-todo":
		return "todo.created", nil
	case "update-todo":
		return "todo.updated", nil
	case "delete-todo":
		return "todo.deleted", nil
	default:
		return "", ErrUnsupportedCommandAction
	}
}

func EventSubject(event contracts.TodoEvent) string {
	return "app.event." + strconv.Itoa(event.ShardID) + ".group." + event.GroupID
}

func ShardFromSubject(entityID, subject string) int {
	parts := strings.Split(subject, ".")
	if len(parts) > 2 {
		if shard, err := strconv.Atoi(parts[2]); err == nil {
			return shard
		}
	}
	return sharding.GetShardID(entityID)
}
