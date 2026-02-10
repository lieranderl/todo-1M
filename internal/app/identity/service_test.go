package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/todo-1m/project/internal/platform/auth"
)

type fakeRepo struct {
	users         map[string]User
	members       map[string]map[string]string
	groups        map[string]Group
	refreshByHash map[string]RefreshToken

	createErr  error
	findErr    error
	memberErr  error
	addUserErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users:         map[string]User{},
		members:       map[string]map[string]string{},
		groups:        map[string]Group{},
		refreshByHash: map[string]RefreshToken{},
	}
}

func (f *fakeRepo) EnsureSchema(ctx context.Context) error { return nil }

func (f *fakeRepo) CreateUser(ctx context.Context, user User) error {
	if f.createErr != nil {
		return f.createErr
	}
	for _, u := range f.users {
		if u.Username == user.Username {
			return errors.New("duplicate")
		}
	}
	f.users[user.ID] = user
	return nil
}

func (f *fakeRepo) FindUserByUsername(ctx context.Context, username string) (User, error) {
	if f.findErr != nil {
		return User{}, f.findErr
	}
	for _, u := range f.users {
		if u.Username == username {
			return u, nil
		}
	}
	return User{}, ErrNotFound
}

func (f *fakeRepo) FindUserByID(ctx context.Context, userID string) (User, error) {
	u, ok := f.users[userID]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) CreateGroup(ctx context.Context, group Group, creatorUserID string) error {
	f.groups[group.ID] = group
	if f.members[group.ID] == nil {
		f.members[group.ID] = map[string]string{}
	}
	f.members[group.ID][creatorUserID] = RoleOwner
	return nil
}

func (f *fakeRepo) AddUserToGroupWithRole(ctx context.Context, groupID, userID, role string) error {
	if f.members[groupID] == nil {
		f.members[groupID] = map[string]string{}
	}
	f.members[groupID][userID] = role
	return nil
}

func (f *fakeRepo) AddUserToGroupByUsernameWithRole(ctx context.Context, groupID, username, role string) error {
	if f.addUserErr != nil {
		return f.addUserErr
	}
	for _, u := range f.users {
		if u.Username == username {
			return f.AddUserToGroupWithRole(ctx, groupID, u.ID, role)
		}
	}
	return ErrNotFound
}

func (f *fakeRepo) SetUserRoleByUsername(ctx context.Context, groupID, username, role string) error {
	for _, u := range f.users {
		if u.Username == username {
			if f.members[groupID] == nil {
				f.members[groupID] = map[string]string{}
			}
			if _, exists := f.members[groupID][u.ID]; !exists {
				return ErrNotFound
			}
			f.members[groupID][u.ID] = role
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeRepo) GetMembershipRole(ctx context.Context, userID, groupID string) (string, error) {
	if f.memberErr != nil {
		return "", f.memberErr
	}
	role := f.members[groupID][userID]
	if role == "" {
		return "", ErrNotFound
	}
	return role, nil
}

func (f *fakeRepo) IsUserInGroup(ctx context.Context, userID, groupID string) (bool, error) {
	if f.memberErr != nil {
		return false, f.memberErr
	}
	_, ok := f.members[groupID][userID]
	return ok, nil
}

func (f *fakeRepo) ListGroupsForUser(ctx context.Context, userID string) ([]GroupMembership, error) {
	result := []GroupMembership{}
	for gid, members := range f.members {
		if role, ok := members[userID]; ok {
			g := f.groups[gid]
			result = append(result, GroupMembership{GroupID: gid, GroupName: g.Name, Role: role})
		}
	}
	return result, nil
}

func (f *fakeRepo) CreateRefreshToken(ctx context.Context, token RefreshToken) error {
	f.refreshByHash[token.TokenHash] = token
	return nil
}

func (f *fakeRepo) FindRefreshTokenByHash(ctx context.Context, tokenHash string) (RefreshToken, error) {
	rt, ok := f.refreshByHash[tokenHash]
	if !ok {
		return RefreshToken{}, ErrNotFound
	}
	if rt.RevokedAt != nil || rt.ExpiresAt.Before(time.Now().UTC()) {
		return RefreshToken{}, ErrNotFound
	}
	return rt, nil
}

func (f *fakeRepo) RevokeRefreshToken(ctx context.Context, tokenID string) error {
	now := time.Now().UTC()
	for hash, rt := range f.refreshByHash {
		if rt.TokenID == tokenID {
			rt.RevokedAt = &now
			f.refreshByHash[hash] = rt
		}
	}
	return nil
}

func testTokenManager() auth.Manager {
	m := auth.NewManager("secret", time.Hour)
	m.Now = func() time.Time { return time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC) }
	return m
}

func TestRegisterLoginRefreshLogout(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, testTokenManager())
	next := 0
	svc.NewID = func() string {
		next++
		return "id-" + string(rune('a'+next))
	}

	reg, err := svc.Register(context.Background(), "Alice", "password123")
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if reg.AccessToken == "" || reg.RefreshToken == "" || reg.UserID == "" {
		t.Fatalf("unexpected register response: %+v", reg)
	}

	login, err := svc.Login(context.Background(), "alice", "password123")
	if err != nil {
		t.Fatalf("Login error: %v", err)
	}
	if login.AccessToken == "" || login.RefreshToken == "" {
		t.Fatalf("unexpected login response: %+v", login)
	}

	refreshed, err := svc.Refresh(context.Background(), login.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh error: %v", err)
	}
	if refreshed.AccessToken == "" || refreshed.RefreshToken == "" {
		t.Fatalf("unexpected refresh response: %+v", refreshed)
	}

	if err := svc.Logout(context.Background(), refreshed.RefreshToken); err != nil {
		t.Fatalf("Logout error: %v", err)
	}
}

func TestAddMemberRolePermissions(t *testing.T) {
	repo := newFakeRepo()
	repo.users["u1"] = User{ID: "u1", Username: "owner"}
	repo.users["u2"] = User{ID: "u2", Username: "bob"}
	repo.groups["g1"] = Group{ID: "g1", Name: "Group"}
	repo.members["g1"] = map[string]string{"u1": RoleOwner}

	svc := NewService(repo, testTokenManager())
	if err := svc.AddMemberByUsername(context.Background(), "u1", "g1", "bob", RoleMember); err != nil {
		t.Fatalf("AddMemberByUsername error: %v", err)
	}
	if role := repo.members["g1"]["u2"]; role != RoleMember {
		t.Fatalf("unexpected role: %s", role)
	}

	repo.members["g1"]["u1"] = RoleMember
	if err := svc.AddMemberByUsername(context.Background(), "u1", "g1", "bob", RoleAdmin); !errors.Is(err, ErrForbiddenRole) {
		t.Fatalf("expected ErrForbiddenRole, got %v", err)
	}
}

func TestUpdateRoleRequiresOwner(t *testing.T) {
	repo := newFakeRepo()
	repo.users["u1"] = User{ID: "u1", Username: "owner"}
	repo.users["u2"] = User{ID: "u2", Username: "bob"}
	repo.groups["g1"] = Group{ID: "g1", Name: "Group"}
	repo.members["g1"] = map[string]string{"u1": RoleOwner, "u2": RoleMember}

	svc := NewService(repo, testTokenManager())
	if err := svc.UpdateMemberRoleByUsername(context.Background(), "u1", "g1", "bob", RoleAdmin); err != nil {
		t.Fatalf("UpdateMemberRoleByUsername error: %v", err)
	}
	if role := repo.members["g1"]["u2"]; role != RoleAdmin {
		t.Fatalf("unexpected role after update: %s", role)
	}

	repo.members["g1"]["u1"] = RoleAdmin
	if err := svc.UpdateMemberRoleByUsername(context.Background(), "u1", "g1", "bob", RoleMember); !errors.Is(err, ErrForbiddenRole) {
		t.Fatalf("expected ErrForbiddenRole, got %v", err)
	}
}
