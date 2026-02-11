package commandapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/todo-1m/project/internal/app/identity"
	"github.com/todo-1m/project/internal/app/query"
	platformauth "github.com/todo-1m/project/internal/platform/auth"
)

type TodoReader interface {
	GetTodoByID(ctx context.Context, todoID string) (query.TodoView, error)
}

type Handler struct {
	Service       *Service
	Identity      *identity.Service
	Todos         TodoReader
	AllowedOrigin string
}

func NewHandler(service *Service, identitySvc *identity.Service, todoReader TodoReader, allowedOrigin string) *Handler {
	return &Handler{
		Service:       service,
		Identity:      identitySvc,
		Todos:         todoReader,
		AllowedOrigin: allowedOrigin,
	}
}

func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(h.corsMiddleware)
	r.Options("/*", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	r.Post("/api/v1/auth/register", h.handleRegister)
	r.Post("/api/v1/auth/login", h.handleLogin)
	r.Post("/api/v1/auth/refresh", h.handleRefresh)
	r.Post("/api/v1/auth/logout", h.handleLogout)

	r.Group(func(authR chi.Router) {
		authR.Use(h.authMiddleware)
		authR.Post("/api/v1/groups", h.handleCreateGroup)
		authR.Delete("/api/v1/groups/{groupID}", h.handleDeleteGroup)
		authR.Post("/api/v1/groups/{groupID}/members", h.handleAddMember)
		authR.Patch("/api/v1/groups/{groupID}/members/role", h.handleUpdateMemberRole)
		authR.Post("/api/v1/command", h.handleCreateCommand)
	})

	return r
}

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type createGroupRequest struct {
	Name string `json:"name"`
}

type addMemberRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

type updateMemberRoleRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

func (h *Handler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	resp, err := h.Identity.Register(r.Context(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidUsername), errors.Is(err, identity.ErrInvalidPassword):
			h.writeError(w, http.StatusBadRequest, err.Error())
		default:
			if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				h.writeError(w, http.StatusConflict, "username already exists")
				return
			}
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	h.writeJSON(w, http.StatusCreated, resp)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	resp, err := h.Identity.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCredentials) {
			h.writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	resp, err := h.Identity.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrRefreshTokenMissing):
			h.writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, identity.ErrInvalidRefreshToken):
			h.writeError(w, http.StatusUnauthorized, err.Error())
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if err := h.Identity.Logout(r.Context(), req.RefreshToken); err != nil {
		if errors.Is(err, identity.ErrRefreshTokenMissing) {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	claims := claimsFromContext(r.Context())
	group, err := h.Identity.CreateGroup(r.Context(), claims.Subject, req.Name)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidGroupName) {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeJSON(w, http.StatusCreated, group)
}

func (h *Handler) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "groupID")
	claims := claimsFromContext(r.Context())
	err := h.Identity.DeleteGroup(r.Context(), claims.Subject, groupID)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidGroupID):
			h.writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, identity.ErrForbiddenGroup), errors.Is(err, identity.ErrForbiddenRole):
			h.writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, identity.ErrNotFound):
			h.writeError(w, http.StatusNotFound, "group not found")
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleAddMember(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "groupID")
	var req addMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	claims := claimsFromContext(r.Context())
	err := h.Identity.AddMemberByUsername(r.Context(), claims.Subject, groupID, req.Username, req.Role)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidGroupID), errors.Is(err, identity.ErrInvalidUsername), errors.Is(err, identity.ErrInvalidRole):
			h.writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, identity.ErrForbiddenGroup), errors.Is(err, identity.ErrForbiddenRole):
			h.writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, identity.ErrNotFound):
			h.writeError(w, http.StatusNotFound, "user not found")
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleUpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "groupID")
	var req updateMemberRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	claims := claimsFromContext(r.Context())
	err := h.Identity.UpdateMemberRoleByUsername(r.Context(), claims.Subject, groupID, req.Username, req.Role)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidGroupID), errors.Is(err, identity.ErrInvalidUsername), errors.Is(err, identity.ErrInvalidRole):
			h.writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, identity.ErrForbiddenGroup), errors.Is(err, identity.ErrForbiddenRole):
			h.writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, identity.ErrNotFound):
			h.writeError(w, http.StatusNotFound, "user not found")
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleCreateCommand(w http.ResponseWriter, r *http.Request) {
	var req CreateTodoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	claims := claimsFromContext(r.Context())
	role, err := h.Identity.EnsureMemberRole(r.Context(), claims.Subject, req.GroupID)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidGroupID):
			h.writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, identity.ErrForbiddenGroup):
			h.writeError(w, http.StatusForbidden, err.Error())
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	action := normalizeAction(req.Action)
	if action == "update-todo" || action == "delete-todo" {
		if h.Todos == nil {
			h.writeError(w, http.StatusInternalServerError, "todo reader is not configured")
			return
		}
		todo, err := h.Todos.GetTodoByID(r.Context(), req.TodoID)
		if err != nil {
			if errors.Is(err, query.ErrTodoNotFound) {
				h.writeError(w, http.StatusNotFound, "todo not found")
				return
			}
			h.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if todo.GroupID != strings.TrimSpace(req.GroupID) {
			h.writeError(w, http.StatusBadRequest, "todo does not belong to group_id")
			return
		}
		if !h.Identity.CanModerateMessages(role) && todo.CreatedByUserID != claims.Subject {
			h.writeError(w, http.StatusForbidden, "insufficient permissions for this action")
			return
		}
	}

	resp, err := h.Service.Accept(Actor{UserID: claims.Subject, Username: claims.Username}, req)
	if err != nil {
		switch {
		case errors.Is(err, ErrTitleRequired), errors.Is(err, ErrGroupRequired), errors.Is(err, ErrTodoIDRequired), errors.Is(err, ErrUnsupportedAction):
			h.writeError(w, http.StatusBadRequest, err.Error())
		default:
			h.writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	h.writeJSON(w, http.StatusAccepted, resp)
}

func (h *Handler) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Origin, Access-Control-Request-Headers")
		w.Header().Set("Access-Control-Allow-Origin", h.allowedOriginForRequest(r.Header.Get("Origin")))
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")

		requestHeaders := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers"))
		if requestHeaders != "" {
			w.Header().Set("Access-Control-Allow-Headers", requestHeaders)
		} else {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, datastar-request")
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) allowedOriginForRequest(requestOrigin string) string {
	allowed := strings.TrimSpace(h.AllowedOrigin)
	if allowed == "" {
		return "*"
	}
	if allowed == "*" {
		return allowed
	}

	origin := strings.TrimSpace(requestOrigin)
	if origin == "" {
		return allowed
	}
	if origin == allowed {
		return origin
	}
	if isEquivalentLoopbackOrigin(origin, allowed) {
		return origin
	}
	return allowed
}

func isEquivalentLoopbackOrigin(originA, originB string) bool {
	a, err := url.Parse(originA)
	if err != nil {
		return false
	}
	b, err := url.Parse(originB)
	if err != nil {
		return false
	}
	if !isLoopbackHost(a.Hostname()) || !isLoopbackHost(b.Hostname()) {
		return false
	}
	if a.Port() != b.Port() {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme)
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

type claimsContextKey struct{}

func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := platformauth.BearerToken(r.Header.Get("Authorization"))
		if token == "" {
			h.writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		claims, err := h.Identity.AuthToken.Parse(token)
		if err != nil {
			h.writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		ctx := r.Context()
		ctx = contextWithClaims(ctx, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, map[string]string{"error": msg})
}

func contextWithClaims(ctx context.Context, claims platformauth.Claims) context.Context {
	return context.WithValue(ctx, claimsContextKey{}, claims)
}

func claimsFromContext(ctx context.Context) platformauth.Claims {
	claims, _ := ctx.Value(claimsContextKey{}).(platformauth.Claims)
	return claims
}
