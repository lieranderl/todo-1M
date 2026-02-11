package query

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrTodoNotFound = errors.New("todo not found")

type TodoView struct {
	TodoID            string     `json:"todo_id"`
	GroupID           string     `json:"group_id"`
	Title             string     `json:"title"`
	CreatedByUserID   string     `json:"created_by_user_id"`
	CreatedByUsername string     `json:"created_by_username"`
	UpdatedByUserID   string     `json:"updated_by_user_id"`
	UpdatedByUsername string     `json:"updated_by_username"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DeletedAt         *time.Time `json:"deleted_at,omitempty"`
}

type TodoRepository struct {
	Pool *pgxpool.Pool
}

func NewTodoRepository(pool *pgxpool.Pool) *TodoRepository {
	return &TodoRepository{Pool: pool}
}

func (r *TodoRepository) ListGroupTodos(ctx context.Context, groupID string, limit int) ([]TodoView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.Pool.Query(ctx,
		`SELECT todo_id, group_id, title,
		        created_by_user_id, created_by_username,
		        updated_by_user_id, updated_by_username,
		        created_at, updated_at, deleted_at
		 FROM todos
		 WHERE group_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at DESC
		 LIMIT $2`,
		groupID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]TodoView, 0, limit)
	for rows.Next() {
		var t TodoView
		if err := rows.Scan(
			&t.TodoID,
			&t.GroupID,
			&t.Title,
			&t.CreatedByUserID,
			&t.CreatedByUsername,
			&t.UpdatedByUserID,
			&t.UpdatedByUsername,
			&t.CreatedAt,
			&t.UpdatedAt,
			&t.DeletedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *TodoRepository) GetTodoByID(ctx context.Context, todoID string) (TodoView, error) {
	var t TodoView
	err := r.Pool.QueryRow(ctx,
		`SELECT todo_id, group_id, title,
		        created_by_user_id, created_by_username,
		        updated_by_user_id, updated_by_username,
		        created_at, updated_at, deleted_at
		 FROM todos
		 WHERE todo_id = $1`,
		todoID,
	).Scan(
		&t.TodoID,
		&t.GroupID,
		&t.Title,
		&t.CreatedByUserID,
		&t.CreatedByUsername,
		&t.UpdatedByUserID,
		&t.UpdatedByUsername,
		&t.CreatedAt,
		&t.UpdatedAt,
		&t.DeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TodoView{}, ErrTodoNotFound
		}
		return TodoView{}, err
	}
	if t.DeletedAt != nil {
		return TodoView{}, ErrTodoNotFound
	}
	return t, nil
}

func (r *TodoRepository) GetGroupProjectionOffset(ctx context.Context, groupID string) (uint64, error) {
	var offset uint64
	err := r.Pool.QueryRow(ctx,
		`SELECT COALESCE(last_event_seq, 0)
		 FROM group_projection_offsets
		 WHERE group_id = $1`,
		groupID,
	).Scan(&offset)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			// Projection offset table is not available yet.
			return 0, nil
		}
		return 0, err
	}
	return offset, nil
}

func (r *TodoRepository) WaitForCommandApplied(ctx context.Context, commandID, groupID string, timeout time.Duration) error {
	commandID = strings.TrimSpace(commandID)
	groupID = strings.TrimSpace(groupID)
	if commandID == "" || groupID == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	deadline := time.Now().Add(timeout)
	delay := 20 * time.Millisecond
	for time.Now().Before(deadline) {
		var marker int
		err := r.Pool.QueryRow(ctx,
			`SELECT 1
			 FROM todo_events
			 WHERE command_id = $1 AND group_id = $2
			 LIMIT 1`,
			commandID, groupID,
		).Scan(&marker)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			var pgErr *pgconn.PgError
			if !(errors.As(err, &pgErr) && pgErr.Code == "42P01") {
				return err
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}

		nextDelay := time.Duration(float64(delay) * 1.5)
		if nextDelay > 250*time.Millisecond {
			nextDelay = 250 * time.Millisecond
		}
		delay = nextDelay
	}
	return nil
}
