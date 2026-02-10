package datasink

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/todo-1m/project/internal/contracts"
)

const createEventsTableSQL = `
CREATE TABLE IF NOT EXISTS todo_events (
  event_id text PRIMARY KEY,
  command_id text NOT NULL,
  entity_id text NOT NULL,
  group_id text NOT NULL,
  actor_user_id text NOT NULL,
  actor_name text NOT NULL DEFAULT '',
  todo_id text NOT NULL DEFAULT '',
  event_type text NOT NULL,
  title text NOT NULL,
  shard_id integer NOT NULL,
  occurred_at timestamptz NOT NULL,
  inserted_at timestamptz NOT NULL DEFAULT now()
)`

const alterTodoEventsGroupIDSQL = `
ALTER TABLE todo_events
ADD COLUMN IF NOT EXISTS group_id text`

const alterTodoEventsActorSQL = `
ALTER TABLE todo_events
ADD COLUMN IF NOT EXISTS actor_user_id text`

const alterTodoEventsEntitySQL = `
ALTER TABLE todo_events
ADD COLUMN IF NOT EXISTS entity_id text`

const alterTodoEventsTodoIDSQL = `
ALTER TABLE todo_events
ADD COLUMN IF NOT EXISTS todo_id text NOT NULL DEFAULT ''`

const alterTodoEventsActorNameSQL = `
ALTER TABLE todo_events
ADD COLUMN IF NOT EXISTS actor_name text NOT NULL DEFAULT ''`

const createTodosTableSQL = `
CREATE TABLE IF NOT EXISTS todos (
  todo_id text PRIMARY KEY,
  group_id text NOT NULL,
  title text NOT NULL,
  created_by_user_id text NOT NULL,
  created_by_username text NOT NULL,
  updated_by_user_id text NOT NULL,
  updated_by_username text NOT NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  deleted_at timestamptz
)`

const createGroupProjectionOffsetsSQL = `
CREATE TABLE IF NOT EXISTS group_projection_offsets (
  group_id text PRIMARY KEY,
  last_event_seq bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now()
)`

const insertEventSQL = `
INSERT INTO todo_events (
  event_id, command_id, entity_id, group_id, actor_user_id, actor_name, todo_id,
  event_type, title, shard_id, occurred_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (event_id) DO NOTHING
`

const upsertTodoCreatedSQL = `
INSERT INTO todos (
  todo_id, group_id, title,
  created_by_user_id, created_by_username,
  updated_by_user_id, updated_by_username,
  created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $4, $5, $6, $6)
ON CONFLICT (todo_id) DO NOTHING
`

const applyTodoUpdatedSQL = `
UPDATE todos
SET title = $2,
    updated_by_user_id = $3,
    updated_by_username = $4,
    updated_at = $5
WHERE todo_id = $1 AND deleted_at IS NULL
`

const applyTodoDeletedSQL = `
UPDATE todos
SET updated_by_user_id = $2,
    updated_by_username = $3,
    updated_at = $4,
    deleted_at = $4
WHERE todo_id = $1 AND deleted_at IS NULL
`

const upsertGroupProjectionOffsetSQL = `
INSERT INTO group_projection_offsets (group_id, last_event_seq, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (group_id) DO UPDATE
SET last_event_seq = GREATEST(group_projection_offsets.last_event_seq, EXCLUDED.last_event_seq),
    updated_at = now()
`

type EventRepository struct {
	Pool *pgxpool.Pool
}

func NewEventRepository(pool *pgxpool.Pool) *EventRepository {
	return &EventRepository{Pool: pool}
}

func (r *EventRepository) EnsureSchema(ctx context.Context) error {
	if _, err := r.Pool.Exec(ctx, createEventsTableSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, alterTodoEventsGroupIDSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, alterTodoEventsActorSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, alterTodoEventsEntitySQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, alterTodoEventsTodoIDSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, alterTodoEventsActorNameSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, createTodosTableSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, createGroupProjectionOffsetsSQL); err != nil {
		return err
	}
	return nil
}

func (r *EventRepository) InsertEvent(ctx context.Context, event contracts.TodoEvent, eventSeq uint64) error {
	tx, err := r.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, insertEventSQL,
		event.EventID,
		event.CommandID,
		event.ActorUserID,
		event.GroupID,
		event.ActorUserID,
		event.ActorName,
		event.TodoID,
		event.EventType,
		event.Title,
		event.ShardID,
		event.OccurredAt,
	); err != nil {
		return err
	}

	switch event.EventType {
	case "todo.created":
		if _, err := tx.Exec(ctx, upsertTodoCreatedSQL,
			event.TodoID,
			event.GroupID,
			event.Title,
			event.ActorUserID,
			event.ActorName,
			event.OccurredAt,
		); err != nil {
			return err
		}
	case "todo.updated":
		if _, err := tx.Exec(ctx, applyTodoUpdatedSQL,
			event.TodoID,
			event.Title,
			event.ActorUserID,
			event.ActorName,
			event.OccurredAt,
		); err != nil {
			return err
		}
	case "todo.deleted":
		if _, err := tx.Exec(ctx, applyTodoDeletedSQL,
			event.TodoID,
			event.ActorUserID,
			event.ActorName,
			event.OccurredAt,
		); err != nil {
			return err
		}
	default:
		return ErrUnsupportedEventType
	}

	if _, err := tx.Exec(ctx, upsertGroupProjectionOffsetSQL, event.GroupID, int64(eventSeq)); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
