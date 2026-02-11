package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/todo-1m/project/internal/app/identity"
	"github.com/todo-1m/project/internal/app/query"
	"github.com/todo-1m/project/internal/contracts"
	platformauth "github.com/todo-1m/project/internal/platform/auth"
	"github.com/todo-1m/project/internal/platform/env"
	"github.com/todo-1m/project/internal/platform/natsutil"
	"github.com/todo-1m/project/services/frontend"
)

func main() {
	ctx := context.Background()
	streamerAddr := env.String("SSE_STREAMER_ADDR", env.DefaultStreamerAddr)
	pgURL := env.String("DATABASE_URL", env.DefaultDatabaseURL)
	jwtSecret := env.String("JWT_SECRET", "dev-insecure-change-me")

	tokenManager := identity.NewTokenManager(jwtSecret)

	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	identityRepo := identity.NewPostgresRepository(pool)
	if err := waitForIdentitySchema(ctx, identityRepo, 30*time.Second); err != nil {
		log.Fatal(err)
	}
	queryRepo := query.NewTodoRepository(pool)

	client, err := natsutil.ConnectJetStreamWithRetry(env.String("NATS_URL", env.DefaultNATSURL), 20*time.Second)
	var nc *nats.Conn
	var js nats.JetStreamContext
	if err != nil {
		log.Println("NATS not connected (running local?):", err)
	} else {
		defer client.Close()
		nc = client.Conn
		js = client.JS
	}

	mux := http.NewServeMux()
	mux.Handle("/", templ.Handler(frontend.LoginPage()))
	mux.Handle("/login", templ.Handler(frontend.LoginPage()))
	mux.Handle("/app", templ.Handler(frontend.WorkspacePage()))
	mux.Handle("/architecture", templ.Handler(frontend.ArchitecturePage()))
	mux.Handle("/settings", templ.Handler(frontend.SettingsPage()))
	mux.Handle("/static/", http.StripPrefix("/static/", frontend.StaticHandler()))

	mux.HandleFunc("/api/v1/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		claims, ok := claimsFromAuthHeader(w, r, tokenManager)
		if !ok {
			return
		}

		groups, err := identityRepo.ListGroupsForUser(r.Context(), claims.Subject)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
	})

	mux.HandleFunc("/api/v1/todos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		claims, ok := claimsFromAuthHeader(w, r, tokenManager)
		if !ok {
			return
		}

		groupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
		if groupID == "" {
			http.Error(w, "group_id is required", http.StatusBadRequest)
			return
		}
		member, err := identityRepo.IsUserInGroup(r.Context(), claims.Subject, groupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !member {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		limit := 50
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				limit = parsed
			}
		}
		todos, err := queryRepo.ListGroupTodos(r.Context(), groupID, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"todos": todos})
	})

	mux.HandleFunc("/api/v1/projection-offset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		claims, ok := claimsFromAuthHeader(w, r, tokenManager)
		if !ok {
			return
		}

		groupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
		if groupID == "" {
			http.Error(w, "group_id is required", http.StatusBadRequest)
			return
		}
		member, err := identityRepo.IsUserInGroup(r.Context(), claims.Subject, groupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !member {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		offset, err := queryRepo.GetGroupProjectionOffset(r.Context(), groupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"group_id":       groupID,
			"last_event_seq": offset,
		})
	})

	mux.HandleFunc("/ui/workspace", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		claims, ok := claimsFromAuthHeader(w, r, tokenManager)
		if !ok {
			return
		}

		groups, err := identityRepo.ListGroupsForUser(r.Context(), claims.Subject)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		activeGroupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
		activeRole := ""
		for _, group := range groups {
			if group.GroupID == activeGroupID {
				activeRole = group.Role
				break
			}
		}
		if activeRole == "" {
			activeGroupID = ""
		}

		todos := make([]query.TodoView, 0)
		if activeGroupID != "" {
			todos, err = queryRepo.ListGroupTodos(r.Context(), activeGroupID, 50)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(renderGroupsList(groups, activeGroupID)))
		_, _ = w.Write([]byte(renderTodoList(todos, claims.Subject, activeRole, activeGroupID)))
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		token := strings.TrimSpace(r.URL.Query().Get("token"))
		if token == "" {
			http.Error(w, "token is required", http.StatusUnauthorized)
			return
		}
		claims, err := tokenManager.Parse(token)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		groupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
		if groupID == "" {
			http.Error(w, "group_id is required", http.StatusBadRequest)
			return
		}
		member, err := identityRepo.IsUserInGroup(r.Context(), claims.Subject, groupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !member {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		sendPatch := func(selector, mode, content string) {
			content = strings.ReplaceAll(content, "\n", "")
			fmt.Fprint(w, "event: datastar-patch-elements\n")
			fmt.Fprintf(w, "data: selector %s\n", selector)
			fmt.Fprintf(w, "data: mode %s\n", mode)
			fmt.Fprintf(w, "data: elements %s\n\n", content)
			flusher.Flush()
		}

		sendFragment := func(msg string, subtitle string) {
			component := frontend.EventItem(msg, subtitle)
			var buf bytes.Buffer
			if err := component.Render(r.Context(), &buf); err != nil {
				return
			}
			sendPatch("#events", "prepend", buf.String())
		}

		role, err := identityRepo.GetMembershipRole(r.Context(), claims.Subject, groupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sendTodos := func(targetEventSeq uint64) {
			waitForProjectionOffset(r.Context(), queryRepo, groupID, targetEventSeq, 2500*time.Millisecond)
			todos, err := queryRepo.ListGroupTodos(r.Context(), groupID, 50)
			if err != nil {
				return
			}
			sendPatch("#todos", "outer", renderTodoList(todos, claims.Subject, role, groupID))
		}

		type streamEvent struct {
			Event contracts.TodoEvent
			Seq   uint64
		}
		eventCh := make(chan streamEvent, 64)
		var sub *nats.Subscription
		if js != nil && nc != nil && nc.IsConnected() {
			sub, err = js.Subscribe("app.event.>", func(msg *nats.Msg) {
				var event contracts.TodoEvent
				if err := json.Unmarshal(msg.Data, &event); err != nil {
					return
				}
				if event.GroupID != groupID {
					return
				}

				var eventSeq uint64
				if meta, metaErr := msg.Metadata(); metaErr == nil {
					eventSeq = meta.Sequence.Stream
				}

				select {
				case eventCh <- streamEvent{Event: event, Seq: eventSeq}:
				default:
				}
			}, nats.DeliverNew())
			if err != nil {
				log.Printf("nats subscribe failed: %v", err)
			}
		}
		if sub != nil {
			defer sub.Unsubscribe()
		}

		sendFragment("Connected to Group Stream!", "Waiting for updates...")
		sendTodos(0)

		for {
			select {
			case <-r.Context().Done():
				return
			case streamEvent := <-eventCh:
				event := streamEvent.Event
				switch event.EventType {
				case "todo.created":
					sendFragment(event.Title, "created by "+event.ActorName)
				case "todo.updated":
					sendFragment(event.Title, "updated by "+event.ActorName)
				case "todo.deleted":
					sendFragment("Todo deleted", "deleted by "+event.ActorName)
				default:
					sendFragment("Group updated", "change by "+event.ActorName)
				}
				sendTodos(streamEvent.Seq)
			}
		}
	})

	fmt.Printf("SSE Streamer listening on %s\n", streamerAddr)
	log.Fatal(http.ListenAndServe(streamerAddr, mux))
}

func waitForIdentitySchema(ctx context.Context, repo *identity.PostgresRepository, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = repo.EnsureSchema(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		log.Printf("waiting for identity schema readiness: %v", lastErr)
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func claimsFromAuthHeader(w http.ResponseWriter, r *http.Request, tokenManager platformauth.Manager) (platformauth.Claims, bool) {
	token := platformauth.BearerToken(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return platformauth.Claims{}, false
	}
	claims, err := tokenManager.Parse(token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return platformauth.Claims{}, false
	}
	return claims, true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func waitForProjectionOffset(ctx context.Context, repo *query.TodoRepository, groupID string, target uint64, timeout time.Duration) {
	if target == 0 {
		return
	}

	deadline := time.Now().Add(timeout)
	delay := 40 * time.Millisecond
	for time.Now().Before(deadline) {
		offset, err := repo.GetGroupProjectionOffset(ctx, groupID)
		if err == nil && offset >= target {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		nextDelay := time.Duration(float64(delay) * 1.5)
		if nextDelay > 320*time.Millisecond {
			nextDelay = 320 * time.Millisecond
		}
		delay = nextDelay
	}
}

func renderGroupsList(groups []identity.GroupMembership, activeGroupID string) string {
	var sb strings.Builder
	sb.WriteString(`<ul id="groups-list" class="menu bg-base-200/60 rounded-box border border-base-300/40 p-2 max-h-64 overflow-auto">`)

	if len(groups) == 0 {
		sb.WriteString(`<li class="text-sm text-base-content/60 px-3 py-2">No groups yet. Create one to start collaborating.</li></ul>`)
		return sb.String()
	}

	for _, group := range groups {
		name := strings.TrimSpace(group.GroupName)
		if name == "" {
			name = group.GroupID
		}

		buttonClass := "w-full text-left rounded-lg px-3 py-2 hover:bg-base-300/40 transition-colors"
		if group.GroupID == activeGroupID {
			buttonClass += " bg-base-300/50"
		}

		sb.WriteString(`<li><div class="flex items-center gap-2">`)
		sb.WriteString(`<button class="`)
		sb.WriteString(html.EscapeString(buttonClass))
		sb.WriteString(`" data-group-id="`)
		sb.WriteString(html.EscapeString(group.GroupID))
		sb.WriteString(`" data-group-role="`)
		sb.WriteString(html.EscapeString(group.Role))
		sb.WriteString(`" data-on:click="@setAll(evt.currentTarget.dataset.groupId, {include: /^active_group_id$/}); @setAll(evt.currentTarget.dataset.groupRole, {include: /^active_group_role$/}); @get('/ui/workspace?group_id=' + evt.currentTarget.dataset.groupId, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}}); @get('/events?group_id=' + evt.currentTarget.dataset.groupId + '&token=' + $access_token, {openWhenHidden: true, filterSignals: {include: /^$/}})">`)
		sb.WriteString(html.EscapeString(name))
		sb.WriteString(` [`)
		sb.WriteString(html.EscapeString(group.Role))
		sb.WriteString(`]</button>`)

		if group.Role == identity.RoleOwner {
			sb.WriteString(`<button class="btn btn-xs btn-error btn-outline shrink-0" data-group-id="`)
			sb.WriteString(html.EscapeString(group.GroupID))
			sb.WriteString(`" data-on:click="evt.stopPropagation(); @setAll((evt.currentTarget.dataset.groupId === $active_group_id) ? '' : $active_group_id, {include: /^active_group_id$/}); @setAll((evt.currentTarget.dataset.groupId === $active_group_id) ? '' : $active_group_role, {include: /^active_group_role$/}); @delete($api_base + '/api/v1/groups/' + evt.currentTarget.dataset.groupId, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}}); @get('/ui/workspace?group_id=' + $active_group_id, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}})">Delete</button>`)
		}
		sb.WriteString(`</div></li>`)
	}

	sb.WriteString(`</ul>`)
	return sb.String()
}

func renderTodoList(todos []query.TodoView, actorUserID, role, activeGroupID string) string {
	var sb strings.Builder
	sb.WriteString(`<div id="todos" class="space-y-3">`)

	if strings.TrimSpace(activeGroupID) == "" {
		sb.WriteString(`<div class="text-sm text-base-content/60 px-2 py-3">Select or connect a group to view todos.</div></div>`)
		return sb.String()
	}

	if len(todos) == 0 {
		sb.WriteString(`<div class="text-sm text-base-content/60 px-2 py-3">No todos in this group yet.</div></div>`)
		return sb.String()
	}

	canModerate := role == identity.RoleOwner || role == identity.RoleAdmin
	for _, todo := range todos {
		meta := "Created by " + strings.TrimSpace(todo.CreatedByUsername)
		if strings.TrimSpace(todo.UpdatedByUsername) != "" && todo.UpdatedByUsername != todo.CreatedByUsername {
			meta += " • Updated by " + strings.TrimSpace(todo.UpdatedByUsername)
		}

		canEdit := canModerate || todo.CreatedByUserID == actorUserID
		inputID := "todo-edit-" + todo.TodoID

		sb.WriteString(`<div class="card bg-base-100 border border-base-300/60 shadow"><div class="card-body p-4 gap-3">`)
		sb.WriteString(`<div class="flex justify-between gap-2 items-start">`)
		sb.WriteString(`<div><div class="font-semibold text-base-content text-base">`)
		sb.WriteString(html.EscapeString(todo.Title))
		sb.WriteString(`</div><div class="text-xs text-base-content/70 mt-1">`)
		sb.WriteString(html.EscapeString(meta))
		sb.WriteString(`</div></div>`)

		if canEdit {
			sb.WriteString(`<div class="join">`)
			sb.WriteString(`<input id="`)
			sb.WriteString(html.EscapeString(inputID))
			sb.WriteString(`" class="input input-bordered input-sm join-item w-52" type="text" value="`)
			sb.WriteString(html.EscapeString(todo.Title))
			sb.WriteString(`"/>`)
			sb.WriteString(`<button class="btn btn-xs btn-outline join-item" data-todo-id="`)
			sb.WriteString(html.EscapeString(todo.TodoID))
			sb.WriteString(`" data-input-id="`)
			sb.WriteString(html.EscapeString(inputID))
			sb.WriteString(`" data-on:click="@post($api_base + '/api/v1/command', {headers: {Authorization: 'Bearer ' + $access_token}, payload: {action: 'update-todo', title: document.getElementById(evt.currentTarget.dataset.inputId).value, group_id: $active_group_id, todo_id: evt.currentTarget.dataset.todoId}, filterSignals: {include: /^$/}}); @get('/ui/workspace?group_id=' + $active_group_id, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}})">Save</button>`)
			sb.WriteString(`<button class="btn btn-xs btn-error btn-outline join-item" data-todo-id="`)
			sb.WriteString(html.EscapeString(todo.TodoID))
			sb.WriteString(`" data-on:click="@post($api_base + '/api/v1/command', {headers: {Authorization: 'Bearer ' + $access_token}, payload: {action: 'delete-todo', title: '', group_id: $active_group_id, todo_id: evt.currentTarget.dataset.todoId}, filterSignals: {include: /^$/}}); @get('/ui/workspace?group_id=' + $active_group_id, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}})">Delete</button>`)
			sb.WriteString(`</div>`)
		} else {
			sb.WriteString(`<span class="badge badge-ghost badge-sm">read-only</span>`)
		}

		sb.WriteString(`</div><div class="divider my-0"></div><div class="flex items-center justify-between text-[11px] text-base-content/60">`)
		sb.WriteString(`<span class="badge badge-ghost badge-sm">group `)
		sb.WriteString(html.EscapeString(todo.GroupID))
		sb.WriteString(`</span><span>todo `)
		sb.WriteString(html.EscapeString(todo.TodoID))
		sb.WriteString(`</span></div></div></div>`)
	}

	sb.WriteString(`</div>`)
	return sb.String()
}
