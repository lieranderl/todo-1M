package datasink

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/todo-1m/project/internal/contracts"
)

var ErrInvalidEventPayload = errors.New("invalid event payload")
var ErrUnsupportedEventType = errors.New("unsupported event type")

type Repository interface {
	InsertEvent(ctx context.Context, event contracts.TodoEvent, eventSeq uint64) error
}

type Service struct {
	Repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{Repository: repository}
}

func (s *Service) Handle(ctx context.Context, payload []byte, eventSeq uint64) error {
	var event contracts.TodoEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return ErrInvalidEventPayload
	}
	return s.Repository.InsertEvent(ctx, event, eventSeq)
}
