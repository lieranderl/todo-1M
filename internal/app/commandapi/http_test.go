package commandapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/todo-1m/project/internal/app/identity"
	"github.com/todo-1m/project/internal/app/query"
	platformauth "github.com/todo-1m/project/internal/platform/auth"
)

type fakeIdentityRepo struct {
	users         map[string]identity.User
	members       map[string]map[string]string
	groups        map[string]identity.Group
	refreshByHash map[string]identity.RefreshToken
}

func newFakeIdentityRepo() *fakeIdentityRepo {
	return &fakeIdentityRepo{
		users:         map[string]identity.User{},
		members:       map[string]map[string]string{},
		groups:        map[string]identity.Group{},
		refreshByHash: map[string]identity.RefreshToken{},
	}
}

func (f *fakeIdentityRepo) EnsureSchema(ctx context.Context) error { return nil }
func (f *fakeIdentityRepo) CreateUser(ctx context.Context, user identity.User) error {
	for _, u := range f.users {
		if u.Username == user.Username {
			return errors.New("duplicate")
		}
	}
	f.users[user.ID] = user
	return nil
}
func (f *fakeIdentityRepo) FindUserByUsername(ctx context.Context, username string) (identity.User, error) {
	for _, u := range f.users {
		if u.Username == username {
			return u, nil
		}
	}
	return identity.User{}, identity.ErrNotFound
}
func (f *fakeIdentityRepo) FindUserByID(ctx context.Context, userID string) (identity.User, error) {
	u, ok := f.users[userID]
	if !ok {
		return identity.User{}, identity.ErrNotFound
	}
	return u, nil
}
func (f *fakeIdentityRepo) CreateGroup(ctx context.Context, group identity.Group, creatorUserID string) error {
	f.groups[group.ID] = group
	if f.members[group.ID] == nil {
		f.members[group.ID] = map[string]string{}
	}
	f.members[group.ID][creatorUserID] = identity.RoleOwner
	return nil
}
func (f *fakeIdentityRepo) AddUserToGroupWithRole(ctx context.Context, groupID, userID, role string) error {
	if f.members[groupID] == nil {
		f.members[groupID] = map[string]string{}
	}
	f.members[groupID][userID] = role
	return nil
}
func (f *fakeIdentityRepo) AddUserToGroupByUsernameWithRole(ctx context.Context, groupID, username, role string) error {
	for _, u := range f.users {
		if u.Username == username {
			return f.AddUserToGroupWithRole(ctx, groupID, u.ID, role)
		}
	}
	return identity.ErrNotFound
}
func (f *fakeIdentityRepo) SetUserRoleByUsername(ctx context.Context, groupID, username, role string) error {
	for _, u := range f.users {
		if u.Username == username {
			if f.members[groupID] == nil {
				f.members[groupID] = map[string]string{}
			}
			if _, ok := f.members[groupID][u.ID]; !ok {
				return identity.ErrNotFound
			}
			f.members[groupID][u.ID] = role
			return nil
		}
	}
	return identity.ErrNotFound
}
func (f *fakeIdentityRepo) GetMembershipRole(ctx context.Context, userID, groupID string) (string, error) {
	role := f.members[groupID][userID]
	if role == "" {
		return "", identity.ErrNotFound
	}
	return role, nil
}
func (f *fakeIdentityRepo) IsUserInGroup(ctx context.Context, userID, groupID string) (bool, error) {
	_, ok := f.members[groupID][userID]
	return ok, nil
}
func (f *fakeIdentityRepo) ListGroupsForUser(ctx context.Context, userID string) ([]identity.GroupMembership, error) {
	out := []identity.GroupMembership{}
	for gid, users := range f.members {
		if role, ok := users[userID]; ok {
			g := f.groups[gid]
			out = append(out, identity.GroupMembership{GroupID: gid, GroupName: g.Name, Role: role})
		}
	}
	return out, nil
}
func (f *fakeIdentityRepo) CreateRefreshToken(ctx context.Context, token identity.RefreshToken) error {
	f.refreshByHash[token.TokenHash] = token
	return nil
}
func (f *fakeIdentityRepo) FindRefreshTokenByHash(ctx context.Context, tokenHash string) (identity.RefreshToken, error) {
	rt, ok := f.refreshByHash[tokenHash]
	if !ok || rt.RevokedAt != nil {
		return identity.RefreshToken{}, identity.ErrNotFound
	}
	return rt, nil
}
func (f *fakeIdentityRepo) RevokeRefreshToken(ctx context.Context, tokenID string) error {
	now := time.Now().UTC()
	for hash, rt := range f.refreshByHash {
		if rt.TokenID == tokenID {
			rt.RevokedAt = &now
			f.refreshByHash[hash] = rt
		}
	}
	return nil
}

type fakeTodoReader struct {
	items map[string]query.TodoView
}

func (f fakeTodoReader) GetTodoByID(ctx context.Context, todoID string) (query.TodoView, error) {
	t, ok := f.items[todoID]
	if !ok {
		return query.TodoView{}, query.ErrTodoNotFound
	}
	return t, nil
}

func newHandlerForTests() (*Handler, *identity.Service) {
	repo := newFakeIdentityRepo()
	repo.users["u1"] = identity.User{ID: "u1", Username: "alice", PasswordHash: "$2a$10$Qdv1fOD2vEUCA6cQbjHqUecFp4Pw1nJ7l/SXxPxq8np5xpoE2mR9a"} // password123
	repo.members["g1"] = map[string]string{"u1": identity.RoleOwner}
	repo.groups["g1"] = identity.Group{ID: "g1", Name: "Group 1"}

	mgr := platformauth.NewManager("secret", time.Hour)
	mgr.Now = func() time.Time { return time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC) }
	identitySvc := identity.NewService(repo, mgr)
	identitySvc.NewID = func() string { return "id-1" }

	svc := NewService(func(_ string, _ []byte) error { return nil })
	svc.NewID = func() string { return "cmd-abc" }

	todos := fakeTodoReader{items: map[string]query.TodoView{
		"todo-1": {TodoID: "todo-1", GroupID: "g1", CreatedByUserID: "u1", Title: "old"},
	}}

	return NewHandler(svc, identitySvc, todos, "http://localhost:8081"), identitySvc
}

func TestHandleCreateCommand_Unauthorized(t *testing.T) {
	handler, _ := newHandlerForTests()

	body, _ := json.Marshal(CreateTodoRequest{Title: "Buy Milk", GroupID: "g1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/command", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	handler.Router().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestHandleCreateCommand_Accepted(t *testing.T) {
	handler, identitySvc := newHandlerForTests()
	token, err := identitySvc.AuthToken.Sign("u1", "alice")
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	body, _ := json.Marshal(CreateTodoRequest{Title: "Buy Milk", GroupID: "g1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/command", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.Router().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp CommandResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	if resp.CommandID != "cmd-abc" || resp.Status != "accepted" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestAuthRegisterLoginRefreshLogout(t *testing.T) {
	handler, _ := newHandlerForTests()

	registerBody := `{"username":"bob","password":"password123"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewBufferString(registerBody))
	handler.Router().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	var reg identity.AuthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &reg); err != nil {
		t.Fatalf("invalid register response: %v", err)
	}

	refreshBody := `{"refresh_token":"` + reg.RefreshToken + `"}`
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytes.NewBufferString(refreshBody))
	handler.Router().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var refreshed identity.AuthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("invalid refresh response: %v", err)
	}

	logoutBody := `{"refresh_token":"` + refreshed.RefreshToken + `"}`
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", bytes.NewBufferString(logoutBody))
	handler.Router().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUpdateCommandForbiddenForNonOwner(t *testing.T) {
	repo := newFakeIdentityRepo()
	repo.users["u2"] = identity.User{ID: "u2", Username: "bob", PasswordHash: "$2a$10$Qdv1fOD2vEUCA6cQbjHqUecFp4Pw1nJ7l/SXxPxq8np5xpoE2mR9a"}
	repo.members["g1"] = map[string]string{"u2": identity.RoleMember}
	repo.groups["g1"] = identity.Group{ID: "g1", Name: "Group 1"}

	mgr := platformauth.NewManager("secret", time.Hour)
	mgr.Now = func() time.Time { return time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC) }
	identitySvc := identity.NewService(repo, mgr)
	identitySvc.NewID = func() string { return "id-1" }

	svc := NewService(func(_ string, _ []byte) error { return nil })
	todos := fakeTodoReader{items: map[string]query.TodoView{
		"todo-1": {TodoID: "todo-1", GroupID: "g1", CreatedByUserID: "u1", Title: "old"},
	}}
	handler := NewHandler(svc, identitySvc, todos, "http://localhost:8081")

	token, err := identitySvc.AuthToken.Sign("u2", "bob")
	if err != nil {
		t.Fatalf("token sign error: %v", err)
	}

	body := `{"action":"update-todo","group_id":"g1","todo_id":"todo-1","title":"new"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/command", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Router().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOptions_HasCORSHeaders(t *testing.T) {
	handler, _ := newHandlerForTests()

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/command", nil)
	rr := httptest.NewRecorder()
	handler.Router().ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8081" {
		t.Fatalf("unexpected CORS origin: %q", got)
	}
}
