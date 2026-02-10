package contracts

import "time"

// TodoCommand is the command published by command-api and processed by domain-engine.
type TodoCommand struct {
	CommandID   string    `json:"command_id"`
	TodoID      string    `json:"todo_id"`
	ActorUserID string    `json:"actor_user_id"`
	ActorName   string    `json:"actor_name"`
	GroupID     string    `json:"group_id"`
	Action      string    `json:"action"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
}

// TodoEvent is the event published by domain-engine and consumed by SSE/data-sink.
type TodoEvent struct {
	EventID     string    `json:"event_id"`
	CommandID   string    `json:"command_id"`
	TodoID      string    `json:"todo_id"`
	GroupID     string    `json:"group_id"`
	ActorUserID string    `json:"actor_user_id"`
	ActorName   string    `json:"actor_name"`
	EventType   string    `json:"event_type"`
	Title       string    `json:"title"`
	OccurredAt  time.Time `json:"occurred_at"`
	ShardID     int       `json:"shard_id"`
}
