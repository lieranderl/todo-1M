package identity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

type User struct {
	ID           string
	Username     string
	PasswordHash string
}

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type GroupMembership struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	Role      string `json:"role"`
}

type RefreshToken struct {
	TokenID   string
	UserID    string
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

type Repository interface {
	EnsureSchema(ctx context.Context) error
	CreateUser(ctx context.Context, user User) error
	FindUserByUsername(ctx context.Context, username string) (User, error)
	FindUserByID(ctx context.Context, userID string) (User, error)

	CreateGroup(ctx context.Context, group Group, creatorUserID string) error
	DeleteGroup(ctx context.Context, groupID string) error
	AddUserToGroupWithRole(ctx context.Context, groupID, userID, role string) error
	AddUserToGroupByUsernameWithRole(ctx context.Context, groupID, username, role string) error
	SetUserRoleByUsername(ctx context.Context, groupID, username, role string) error
	GetMembershipRole(ctx context.Context, userID, groupID string) (string, error)
	IsUserInGroup(ctx context.Context, userID, groupID string) (bool, error)
	ListGroupsForUser(ctx context.Context, userID string) ([]GroupMembership, error)

	CreateRefreshToken(ctx context.Context, token RefreshToken) error
	FindRefreshTokenByHash(ctx context.Context, tokenHash string) (RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, tokenID string) error
}

type PostgresRepository struct {
	Pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{Pool: pool}
}

const createUsersSQL = `
CREATE TABLE IF NOT EXISTS users (
  id text PRIMARY KEY,
  username text NOT NULL UNIQUE,
  password_hash text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
)`

const createGroupsSQL = `
CREATE TABLE IF NOT EXISTS groups (
  id text PRIMARY KEY,
  name text NOT NULL,
  created_by text NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now()
)`

const createGroupMembersSQL = `
CREATE TABLE IF NOT EXISTS group_members (
  group_id text NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role text NOT NULL DEFAULT 'member',
  added_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (group_id, user_id)
)`

const alterGroupMembersRoleSQL = `
ALTER TABLE group_members
ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'member'`

const createRefreshTokensSQL = `
CREATE TABLE IF NOT EXISTS refresh_tokens (
  token_id text PRIMARY KEY,
  user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash text NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
)`

func (r *PostgresRepository) EnsureSchema(ctx context.Context) error {
	if _, err := r.Pool.Exec(ctx, createUsersSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, createGroupsSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, createGroupMembersSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, alterGroupMembersRoleSQL); err != nil {
		return err
	}
	if _, err := r.Pool.Exec(ctx, createRefreshTokensSQL); err != nil {
		return err
	}
	return nil
}

func (r *PostgresRepository) CreateUser(ctx context.Context, user User) error {
	_, err := r.Pool.Exec(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES ($1, $2, $3)`,
		user.ID, user.Username, user.PasswordHash,
	)
	return err
}

func (r *PostgresRepository) FindUserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := r.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash FROM users WHERE username = $1`,
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return u, nil
}

func (r *PostgresRepository) FindUserByID(ctx context.Context, userID string) (User, error) {
	var u User
	err := r.Pool.QueryRow(ctx,
		`SELECT id, username, password_hash FROM users WHERE id = $1`,
		userID,
	).Scan(&u.ID, &u.Username, &u.PasswordHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return u, nil
}

func (r *PostgresRepository) CreateGroup(ctx context.Context, group Group, creatorUserID string) error {
	tx, err := r.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO groups (id, name, created_by) VALUES ($1, $2, $3)`,
		group.ID, group.Name, creatorUserID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id, role)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (group_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		group.ID, creatorUserID, RoleOwner,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (r *PostgresRepository) DeleteGroup(ctx context.Context, groupID string) error {
	res, err := r.Pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) AddUserToGroupWithRole(ctx context.Context, groupID, userID, role string) error {
	_, err := r.Pool.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id, role)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (group_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		groupID, userID, role,
	)
	return err
}

func (r *PostgresRepository) AddUserToGroupByUsernameWithRole(ctx context.Context, groupID, username, role string) error {
	var userID string
	err := r.Pool.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return r.AddUserToGroupWithRole(ctx, groupID, userID, role)
}

func (r *PostgresRepository) SetUserRoleByUsername(ctx context.Context, groupID, username, role string) error {
	res, err := r.Pool.Exec(ctx,
		`UPDATE group_members gm
		 SET role = $3
		 FROM users u
		 WHERE gm.group_id = $1 AND gm.user_id = u.id AND u.username = $2`,
		groupID, username, role,
	)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) GetMembershipRole(ctx context.Context, userID, groupID string) (string, error) {
	var role string
	err := r.Pool.QueryRow(ctx,
		`SELECT role FROM group_members WHERE group_id = $1 AND user_id = $2`,
		groupID, userID,
	).Scan(&role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return role, nil
}

func (r *PostgresRepository) IsUserInGroup(ctx context.Context, userID, groupID string) (bool, error) {
	var exists bool
	err := r.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM group_members WHERE group_id = $1 AND user_id = $2)`,
		groupID, userID,
	).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) ListGroupsForUser(ctx context.Context, userID string) ([]GroupMembership, error) {
	rows, err := r.Pool.Query(ctx,
		`SELECT g.id, g.name, gm.role
		 FROM groups g
		 INNER JOIN group_members gm ON gm.group_id = g.id
		 WHERE gm.user_id = $1
		 ORDER BY g.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := make([]GroupMembership, 0)
	for rows.Next() {
		var g GroupMembership
		if err := rows.Scan(&g.GroupID, &g.GroupName, &g.Role); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func (r *PostgresRepository) CreateRefreshToken(ctx context.Context, token RefreshToken) error {
	_, err := r.Pool.Exec(ctx,
		`INSERT INTO refresh_tokens (token_id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
		token.TokenID, token.UserID, token.TokenHash, token.ExpiresAt,
	)
	return err
}

func (r *PostgresRepository) FindRefreshTokenByHash(ctx context.Context, tokenHash string) (RefreshToken, error) {
	var rt RefreshToken
	err := r.Pool.QueryRow(ctx,
		`SELECT token_id, user_id, token_hash, expires_at, revoked_at
		 FROM refresh_tokens
		 WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`,
		tokenHash,
	).Scan(&rt.TokenID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt, &rt.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefreshToken{}, ErrNotFound
		}
		return RefreshToken{}, err
	}
	return rt, nil
}

func (r *PostgresRepository) RevokeRefreshToken(ctx context.Context, tokenID string) error {
	_, err := r.Pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE token_id = $1`,
		tokenID,
	)
	return err
}
