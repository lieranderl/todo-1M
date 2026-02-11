package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/nats-io/nuid"
	"github.com/todo-1m/project/internal/platform/auth"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidUsername     = errors.New("username is required")
	ErrInvalidPassword     = errors.New("password must be at least 8 characters")
	ErrInvalidGroupName    = errors.New("group name is required")
	ErrInvalidGroupID      = errors.New("group_id is required")
	ErrInvalidRole         = errors.New("invalid role")
	ErrInvalidCredentials  = errors.New("invalid credentials")
	ErrForbiddenGroup      = errors.New("user is not a member of the group")
	ErrForbiddenRole       = errors.New("insufficient permissions for this action")
	ErrRefreshTokenMissing = errors.New("refresh_token is required")
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
)

type AuthResponse struct {
	Token        string `json:"token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	Username     string `json:"username"`
}

type Service struct {
	Repo       Repository
	AuthToken  auth.Manager
	NewID      func() string
	RefreshTTL time.Duration
	Now        func() time.Time
}

func NewService(repo Repository, tokenManager auth.Manager) *Service {
	return &Service{
		Repo:       repo,
		AuthToken:  tokenManager,
		NewID:      nuid.Next,
		RefreshTTL: 30 * 24 * time.Hour,
		Now:        func() time.Time { return time.Now().UTC() },
	}
}

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func validateCredentials(username, password string) error {
	if normalizeUsername(username) == "" {
		return ErrInvalidUsername
	}
	if len(strings.TrimSpace(password)) < 8 {
		return ErrInvalidPassword
	}
	return nil
}

func IsValidRole(role string) bool {
	switch strings.TrimSpace(role) {
	case RoleOwner, RoleAdmin, RoleMember:
		return true
	default:
		return false
	}
}

func canManageMembers(role string) bool {
	return role == RoleOwner || role == RoleAdmin
}

func canManageRoles(role string) bool {
	return role == RoleOwner
}

func canModerateMessages(role string) bool {
	return role == RoleOwner || role == RoleAdmin
}

func (s *Service) Register(ctx context.Context, username, password string) (AuthResponse, error) {
	if err := validateCredentials(username, password); err != nil {
		return AuthResponse{}, err
	}
	uname := normalizeUsername(username)

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return AuthResponse{}, err
	}

	u := User{
		ID:           s.NewID(),
		Username:     uname,
		PasswordHash: string(hash),
	}
	if err := s.Repo.CreateUser(ctx, u); err != nil {
		return AuthResponse{}, err
	}
	return s.issueSession(ctx, u)
}

func (s *Service) Login(ctx context.Context, username, password string) (AuthResponse, error) {
	uname := normalizeUsername(username)
	if uname == "" || strings.TrimSpace(password) == "" {
		return AuthResponse{}, ErrInvalidCredentials
	}

	u, err := s.Repo.FindUserByUsername(ctx, uname)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return AuthResponse{}, ErrInvalidCredentials
		}
		return AuthResponse{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return AuthResponse{}, ErrInvalidCredentials
	}
	return s.issueSession(ctx, u)
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (AuthResponse, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return AuthResponse{}, ErrRefreshTokenMissing
	}

	session, err := s.Repo.FindRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return AuthResponse{}, ErrInvalidRefreshToken
		}
		return AuthResponse{}, err
	}
	if err := s.Repo.RevokeRefreshToken(ctx, session.TokenID); err != nil {
		return AuthResponse{}, err
	}

	u, err := s.Repo.FindUserByID(ctx, session.UserID)
	if err != nil {
		return AuthResponse{}, err
	}
	return s.issueSession(ctx, u)
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return ErrRefreshTokenMissing
	}
	session, err := s.Repo.FindRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return s.Repo.RevokeRefreshToken(ctx, session.TokenID)
}

func (s *Service) CreateGroup(ctx context.Context, actorUserID, name string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, ErrInvalidGroupName
	}
	g := Group{ID: s.NewID(), Name: name}
	if err := s.Repo.CreateGroup(ctx, g, actorUserID); err != nil {
		return Group{}, err
	}
	return g, nil
}

func (s *Service) DeleteGroup(ctx context.Context, actorUserID, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return ErrInvalidGroupID
	}

	actorRole, err := s.Repo.GetMembershipRole(ctx, actorUserID, groupID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrForbiddenGroup
		}
		return err
	}
	if actorRole != RoleOwner {
		return ErrForbiddenRole
	}

	return s.Repo.DeleteGroup(ctx, groupID)
}

func (s *Service) AddMemberByUsername(ctx context.Context, actorUserID, groupID, username, role string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return ErrInvalidGroupID
	}
	username = normalizeUsername(username)
	if username == "" {
		return ErrInvalidUsername
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = RoleMember
	}
	if !IsValidRole(role) || role == RoleOwner {
		return ErrInvalidRole
	}

	actorRole, err := s.Repo.GetMembershipRole(ctx, actorUserID, groupID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrForbiddenGroup
		}
		return err
	}
	if !canManageMembers(actorRole) {
		return ErrForbiddenRole
	}
	if actorRole != RoleOwner && role == RoleAdmin {
		return ErrForbiddenRole
	}

	return s.Repo.AddUserToGroupByUsernameWithRole(ctx, groupID, username, role)
}

func (s *Service) UpdateMemberRoleByUsername(ctx context.Context, actorUserID, groupID, username, role string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return ErrInvalidGroupID
	}
	username = normalizeUsername(username)
	if username == "" {
		return ErrInvalidUsername
	}
	role = strings.TrimSpace(role)
	if !IsValidRole(role) || role == RoleOwner {
		return ErrInvalidRole
	}

	actorRole, err := s.Repo.GetMembershipRole(ctx, actorUserID, groupID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrForbiddenGroup
		}
		return err
	}
	if !canManageRoles(actorRole) {
		return ErrForbiddenRole
	}
	return s.Repo.SetUserRoleByUsername(ctx, groupID, username, role)
}

func (s *Service) ListGroups(ctx context.Context, userID string) ([]GroupMembership, error) {
	return s.Repo.ListGroupsForUser(ctx, userID)
}

func (s *Service) EnsureMemberRole(ctx context.Context, userID, groupID string) (string, error) {
	if strings.TrimSpace(groupID) == "" {
		return "", ErrInvalidGroupID
	}
	role, err := s.Repo.GetMembershipRole(ctx, userID, groupID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", ErrForbiddenGroup
		}
		return "", err
	}
	return role, nil
}

func (s *Service) CanModerateMessages(role string) bool {
	return canModerateMessages(role)
}

func (s *Service) issueSession(ctx context.Context, user User) (AuthResponse, error) {
	accessToken, err := s.AuthToken.Sign(user.ID, user.Username)
	if err != nil {
		return AuthResponse{}, err
	}

	refreshToken := s.NewID() + "." + s.NewID()
	session := RefreshToken{
		TokenID:   s.NewID(),
		UserID:    user.ID,
		TokenHash: hashRefreshToken(refreshToken),
		ExpiresAt: s.Now().Add(s.RefreshTTL),
	}
	if err := s.Repo.CreateRefreshToken(ctx, session); err != nil {
		return AuthResponse{}, err
	}

	return AuthResponse{
		Token:        accessToken,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UserID:       user.ID,
		Username:     user.Username,
	}, nil
}

func hashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func NewTokenManager(secret string) auth.Manager {
	m := auth.NewManager(secret, 15*time.Minute)
	return m
}
