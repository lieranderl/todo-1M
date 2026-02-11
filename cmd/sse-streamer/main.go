package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/todo-1m/project/internal/app/identity"
	"github.com/todo-1m/project/internal/app/query"
	"github.com/todo-1m/project/internal/contracts"
	platformauth "github.com/todo-1m/project/internal/platform/auth"
	"github.com/todo-1m/project/internal/platform/dbpool"
	"github.com/todo-1m/project/internal/platform/env"
	"github.com/todo-1m/project/internal/platform/natsutil"
	"github.com/todo-1m/project/services/frontend"
)

var userStreams = newUserStreamRegistry()
var groupStreams *groupStreamRegistry

func main() {
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	streamerAddr := env.String("SSE_STREAMER_ADDR", env.DefaultStreamerAddr)
	pgURL := env.String("DATABASE_URL", env.DefaultDatabaseURL)
	jwtSecret := env.String("JWT_SECRET", "dev-insecure-change-me")
	shutdownTimeout := env.Duration("SHUTDOWN_TIMEOUT", 10*time.Second)

	tokenManager := identity.NewTokenManager(jwtSecret)

	pool, err := dbpool.New(runCtx, pgURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	identityRepo := identity.NewPostgresRepository(pool)
	if err := waitForIdentitySchema(runCtx, identityRepo, 30*time.Second); err != nil {
		log.Fatal(err)
	}
	queryRepo := query.NewTodoRepository(pool)

	client, err := natsutil.ConnectJetStreamWithRetry(env.String("NATS_URL", env.DefaultNATSURL), env.Duration("NATS_CONNECT_TIMEOUT", 90*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	js := client.JS
	groupStreams = newGroupStreamRegistry(js, queryRepo)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := checkSSEStreamerReadiness(r.Context(), pool, client.Conn); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
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

		token := platformauth.BearerToken(r.Header.Get("Authorization"))
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		if token == "" {
			http.Error(w, "token is required", http.StatusUnauthorized)
			return
		}
		claims, err := tokenManager.Parse(token)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		streamCtx, cancelStream := context.WithCancel(r.Context())
		streamID := fmt.Sprintf("%d", time.Now().UnixNano())
		if cancelPrev := userStreams.Replace(claims.Subject, streamID, cancelStream); cancelPrev != nil {
			cancelPrev()
		}
		defer userStreams.Release(claims.Subject, streamID)
		defer cancelStream()

		groupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
		if groupID == "" {
			http.Error(w, "group_id is required", http.StatusBadRequest)
			return
		}
		member, err := identityRepo.IsUserInGroup(streamCtx, claims.Subject, groupID)
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

		role, err := identityRepo.GetMembershipRole(streamCtx, claims.Subject, groupID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		todoSelector := cssAttrSelector("todos", "data-group-id", groupID)
		eventsSelector := cssAttrSelector("events", "data-group-id", groupID)
		sendFragment := func(msg string, subtitle string) {
			component := frontend.EventItem(msg, subtitle)
			var buf bytes.Buffer
			if err := component.Render(streamCtx, &buf); err != nil {
				return
			}
			sendPatch(eventsSelector, "prepend", buf.String())
		}
		eventCh, unsubscribeGroup, err := groupStreams.Subscribe(groupID)
		if err != nil {
			http.Error(w, "stream subscription failed", http.StatusInternalServerError)
			return
		}
		defer unsubscribeGroup()

		sendPatch("#todos", "outer", renderTodoList(nil, claims.Subject, role, groupID))
		sendPatch("#events", "outer", renderEventsContainer(groupID))
		sendFragment("Connected to Group Stream!", "Waiting for updates...")
		if initialTodos, err := queryRepo.ListGroupTodos(streamCtx, groupID, 50); err == nil {
			sendPatch(todoSelector, "outer", renderTodoList(initialTodos, claims.Subject, role, groupID))
		}

		for {
			select {
			case <-streamCtx.Done():
				return
			case streamMsg := <-eventCh:
				if streamMsg.Event != nil {
					event := *streamMsg.Event
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
				}
				if streamMsg.Todos != nil {
					sendPatch(todoSelector, "outer", renderTodoList(streamMsg.Todos, claims.Subject, role, groupID))
				}
			}
		}
	})

	mux.HandleFunc("/events/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		token := platformauth.BearerToken(r.Header.Get("Authorization"))
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		if token == "" {
			http.Error(w, "token is required", http.StatusUnauthorized)
			return
		}

		claims, err := tokenManager.Parse(token)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		userStreams.Cancel(claims.Subject)
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:              streamerAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Keep WriteTimeout unset for long-lived SSE streams.
		IdleTimeout: 120 * time.Second,
	}

	fmt.Printf("SSE Streamer listening on %s\n", streamerAddr)
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		log.Fatal(err)
	case <-runCtx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("sse-streamer graceful shutdown failed: %v", err)
	}
}

type userStreamLease struct {
	id     string
	cancel context.CancelFunc
}

type userStreamRegistry struct {
	mu     sync.Mutex
	byUser map[string]userStreamLease
}

func newUserStreamRegistry() *userStreamRegistry {
	return &userStreamRegistry{byUser: make(map[string]userStreamLease)}
}

func (r *userStreamRegistry) Replace(userID, streamID string, cancel context.CancelFunc) context.CancelFunc {
	r.mu.Lock()
	defer r.mu.Unlock()

	var prevCancel context.CancelFunc
	if current, ok := r.byUser[userID]; ok {
		prevCancel = current.cancel
	}
	r.byUser[userID] = userStreamLease{id: streamID, cancel: cancel}
	return prevCancel
}

func (r *userStreamRegistry) Release(userID, streamID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.byUser[userID]
	if !ok {
		return
	}
	if current.id != streamID {
		return
	}
	delete(r.byUser, userID)
}

func (r *userStreamRegistry) Cancel(userID string) {
	r.mu.Lock()
	lease, ok := r.byUser[userID]
	if ok {
		delete(r.byUser, userID)
	}
	r.mu.Unlock()

	if ok && lease.cancel != nil {
		lease.cancel()
	}
}

type groupStreamMessage struct {
	Event *contracts.TodoEvent
	Seq   uint64
	Todos []query.TodoView
}

type groupStreamRegistry struct {
	mu      sync.Mutex
	js      nats.JetStreamContext
	repo    *query.TodoRepository
	byGroup map[string]*groupStream
}

type groupStream struct {
	groupID string
	js      nats.JetStreamContext
	repo    *query.TodoRepository

	mu           sync.Mutex
	sub          *nats.Subscription
	subscribers  map[string]chan groupStreamMessage
	nextID       uint64
	pendingSeq   uint64
	refreshTimer *time.Timer
}

func newGroupStreamRegistry(js nats.JetStreamContext, repo *query.TodoRepository) *groupStreamRegistry {
	return &groupStreamRegistry{
		js:      js,
		repo:    repo,
		byGroup: map[string]*groupStream{},
	}
}

func (r *groupStreamRegistry) Subscribe(groupID string) (<-chan groupStreamMessage, func(), error) {
	r.mu.Lock()
	stream, ok := r.byGroup[groupID]
	if !ok {
		stream = &groupStream{
			groupID:      groupID,
			js:           r.js,
			repo:         r.repo,
			subscribers:  map[string]chan groupStreamMessage{},
			pendingSeq:   0,
			refreshTimer: nil,
		}
		r.byGroup[groupID] = stream
	}
	r.mu.Unlock()

	subID, ch, err := stream.addSubscriber()
	if err != nil {
		return nil, nil, err
	}

	unsubscribe := func() {
		empty := stream.removeSubscriber(subID)
		if !empty {
			return
		}
		r.mu.Lock()
		current, ok := r.byGroup[groupID]
		if ok && current == stream {
			delete(r.byGroup, groupID)
		}
		r.mu.Unlock()
	}

	return ch, unsubscribe, nil
}

func (s *groupStream) addSubscriber() (string, chan groupStreamMessage, error) {
	ch := make(chan groupStreamMessage, 64)

	s.mu.Lock()
	s.nextID++
	subID := fmt.Sprintf("%s-%d", s.groupID, s.nextID)
	s.subscribers[subID] = ch
	s.mu.Unlock()

	if err := s.ensureSubscription(); err != nil {
		s.mu.Lock()
		delete(s.subscribers, subID)
		s.mu.Unlock()
		return "", nil, err
	}

	return subID, ch, nil
}

func (s *groupStream) removeSubscriber(subID string) bool {
	var (
		shouldStop bool
		sub        *nats.Subscription
		timer      *time.Timer
	)

	s.mu.Lock()
	delete(s.subscribers, subID)
	if len(s.subscribers) == 0 {
		shouldStop = true
		sub = s.sub
		timer = s.refreshTimer
		s.sub = nil
		s.refreshTimer = nil
		s.pendingSeq = 0
	}
	s.mu.Unlock()

	if shouldStop {
		if timer != nil {
			timer.Stop()
		}
		if sub != nil {
			_ = sub.Unsubscribe()
		}
	}

	return shouldStop
}

func (s *groupStream) ensureSubscription() error {
	s.mu.Lock()
	if s.sub != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	if s.js == nil {
		return fmt.Errorf("jetstream is not configured")
	}

	sub, err := s.js.Subscribe(groupEventSubject(s.groupID), func(msg *nats.Msg) {
		var event contracts.TodoEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			return
		}

		var eventSeq uint64
		if meta, metaErr := msg.Metadata(); metaErr == nil {
			eventSeq = meta.Sequence.Stream
		}

		s.broadcast(groupStreamMessage{Event: &event, Seq: eventSeq})
		s.scheduleSnapshot(eventSeq)
	}, nats.DeliverNew())
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.sub != nil {
		s.mu.Unlock()
		_ = sub.Unsubscribe()
		return nil
	}
	s.sub = sub
	s.mu.Unlock()
	return nil
}

func (s *groupStream) broadcast(msg groupStreamMessage) {
	s.mu.Lock()
	subs := make([]chan groupStreamMessage, 0, len(s.subscribers))
	for _, ch := range s.subscribers {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *groupStream) scheduleSnapshot(seq uint64) {
	const snapshotDebounce = 75 * time.Millisecond

	s.mu.Lock()
	if seq > s.pendingSeq {
		s.pendingSeq = seq
	}
	if s.refreshTimer == nil {
		s.refreshTimer = time.AfterFunc(snapshotDebounce, s.runSnapshotRefresh)
		s.mu.Unlock()
		return
	}
	s.refreshTimer.Reset(snapshotDebounce)
	s.mu.Unlock()
}

func (s *groupStream) runSnapshotRefresh() {
	s.mu.Lock()
	targetSeq := s.pendingSeq
	s.pendingSeq = 0
	s.refreshTimer = nil
	hasSubscribers := len(s.subscribers) > 0
	s.mu.Unlock()

	if !hasSubscribers {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	waitForProjectionOffset(ctx, s.repo, s.groupID, targetSeq, 2500*time.Millisecond)
	todos, err := s.repo.ListGroupTodos(ctx, s.groupID, 50)
	if err != nil {
		return
	}

	s.broadcast(groupStreamMessage{Seq: targetSeq, Todos: todos})
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

func checkSSEStreamerReadiness(ctx context.Context, pool *pgxpool.Pool, conn *nats.Conn) error {
	if conn == nil {
		return errors.New("nats connection is nil")
	}
	if conn.Status() != nats.CONNECTED {
		return fmt.Errorf("nats is not connected: %s", conn.Status().String())
	}

	checkCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := pool.Ping(checkCtx); err != nil {
		return fmt.Errorf("postgres ping failed: %w", err)
	}
	return nil
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

func groupEventSubject(groupID string) string {
	return "app.event.*.group." + groupID
}

func cssAttrSelector(id, attr, value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return fmt.Sprintf("#%s[%s='%s']", id, attr, escaped)
}

func renderGroupsList(groups []identity.GroupMembership, activeGroupID string) string {
	var sb strings.Builder
	sb.WriteString(`<ul id="groups-list" class="list bg-base-200/60 rounded-box border border-base-300/40 p-2 max-h-64 overflow-auto" data-on-load="window.location.pathname === '/app' && $access_token && (() => { const connectButtons = Array.from(document.querySelectorAll('#groups-list button[data-connect-btn=\'1\'][data-group-id][data-group-name]')); if (connectButtons.length === 0) { const groupsListText = (document.getElementById('groups-list')?.textContent || '').toLowerCase(); if (groupsListText.includes('no groups yet')) { @setAll('', {include: /^(active_group_id|active_group_role|connected_group_id|connected_group_name)$/}); } return; } const byID = new Map(connectButtons.map((btn) => [btn.dataset.groupId || '', btn])); let targetID = $connected_group_id; if (!targetID || !byID.has(targetID)) { targetID = byID.has($active_group_id) ? $active_group_id : (connectButtons[0].dataset.groupId || ''); } if (!targetID) { return; } const targetButton = byID.get(targetID); if (!targetButton) { return; } @setAll(targetID, {include: /^active_group_id$/}); @setAll(targetButton.dataset.groupRole || '', {include: /^active_group_role$/}); @setAll(targetID, {include: /^connected_group_id$/}); @setAll(targetButton.dataset.groupName || '', {include: /^connected_group_name$/}); })()">`)

	if len(groups) == 0 {
		sb.WriteString(`<li class="list-row text-sm text-base-content/60">No groups yet. Create one to start collaborating.</li></ul>`)
		return sb.String()
	}

	for _, group := range groups {
		name := strings.TrimSpace(group.GroupName)
		if name == "" {
			name = "Untitled group"
		}

		nameClass := "block w-full min-w-0 rounded-lg px-3 py-2 text-left leading-tight text-base-content"
		if group.GroupID == activeGroupID {
			nameClass += " bg-base-300/50"
		}

		sb.WriteString(`<li class="list-row items-center">`)
		sb.WriteString(`<div class="list-col-grow min-w-0">`)
		sb.WriteString(`<label class="`)
		sb.WriteString(html.EscapeString(nameClass))
		sb.WriteString(`">`)
		sb.WriteString(`<span class="block truncate">`)
		sb.WriteString(html.EscapeString(name))
		sb.WriteString(`</span><span class="text-xs text-base-content/70">`)
		sb.WriteString(html.EscapeString(group.Role))
		sb.WriteString(`</span></label>`)
		sb.WriteString(`</div>`)

		sb.WriteString(`<button class="btn btn-sm btn-secondary btn-outline w-24" data-connect-btn="1" data-group-id="`)
		sb.WriteString(html.EscapeString(group.GroupID))
		sb.WriteString(`" data-group-name="`)
		sb.WriteString(html.EscapeString(name))
		sb.WriteString(`" data-group-role="`)
		sb.WriteString(html.EscapeString(group.Role))
		sb.WriteString(`" data-on:click="@setAll(evt.currentTarget.dataset.groupId, {include: /^active_group_id$/}); @setAll(evt.currentTarget.dataset.groupRole, {include: /^active_group_role$/}); @setAll(evt.currentTarget.dataset.groupId, {include: /^connected_group_id$/}); @setAll(evt.currentTarget.dataset.groupName, {include: /^connected_group_name$/}); @get('/ui/workspace?group_id=' + evt.currentTarget.dataset.groupId, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}})">Connect</button>`)

		if group.Role == identity.RoleOwner {
			sb.WriteString(`<button class="btn btn-sm btn-error btn-outline w-24" data-indicator:delete_group_busy data-attr:disabled="$delete_group_busy" data-group-id="`)
			sb.WriteString(html.EscapeString(group.GroupID))
			sb.WriteString(`" data-on:click="evt.stopPropagation(); @setAll(true, {include: /^groups_dirty$/}); @setAll((evt.currentTarget.dataset.groupId === $active_group_id) ? '' : $active_group_id, {include: /^active_group_id$/}); @setAll((evt.currentTarget.dataset.groupId === $active_group_id) ? '' : $active_group_role, {include: /^active_group_role$/}); @setAll((evt.currentTarget.dataset.groupId === $connected_group_id) ? '' : $connected_group_id, {include: /^connected_group_id$/}); @setAll((evt.currentTarget.dataset.groupId === $connected_group_id) ? '' : $connected_group_name, {include: /^connected_group_name$/}); evt.currentTarget.dataset.groupId === $connected_group_id && @get('/events/disconnect', {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}}); @delete($api_base + '/api/v1/groups/' + evt.currentTarget.dataset.groupId, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}})">Delete</button>`)
		} else {
			sb.WriteString(`<span class="btn btn-sm btn-ghost w-24 invisible" aria-hidden="true">Delete</span>`)
		}
		sb.WriteString(`</li>`)
	}

	sb.WriteString(`</ul>`)
	return sb.String()
}

func renderTodoList(todos []query.TodoView, actorUserID, role, activeGroupID string) string {
	var sb strings.Builder
	sb.WriteString(`<div id="todos" data-group-id="`)
	sb.WriteString(html.EscapeString(strings.TrimSpace(activeGroupID)))
	sb.WriteString(`" class="space-y-3">`)

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
		sb.WriteString(`<div class="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">`)
		sb.WriteString(`<div><div class="font-semibold text-base-content text-base">`)
		sb.WriteString(html.EscapeString(todo.Title))
		sb.WriteString(`</div><div class="text-xs text-base-content/70 mt-1">`)
		sb.WriteString(html.EscapeString(meta))
		sb.WriteString(`</div></div>`)

		if canEdit {
			sb.WriteString(`<div class="grid w-full gap-2 sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:items-center lg:max-w-[28rem]">`)
			sb.WriteString(`<input id="`)
			sb.WriteString(html.EscapeString(inputID))
			sb.WriteString(`" class="input input-bordered input-sm w-full min-w-0" type="text" value="`)
			sb.WriteString(html.EscapeString(todo.Title))
			sb.WriteString(`"/>`)
			sb.WriteString(`<button class="btn btn-sm btn-outline" data-todo-id="`)
			sb.WriteString(html.EscapeString(todo.TodoID))
			sb.WriteString(`" data-input-id="`)
			sb.WriteString(html.EscapeString(inputID))
			sb.WriteString(`" data-on:click="@post($api_base + '/api/v1/command', {headers: {Authorization: 'Bearer ' + $access_token}, payload: {action: 'update-todo', title: document.getElementById(evt.currentTarget.dataset.inputId).value, group_id: $active_group_id, todo_id: evt.currentTarget.dataset.todoId}, filterSignals: {include: /^$/}})">Save</button>`)
			sb.WriteString(`<button class="btn btn-sm btn-error btn-outline" data-todo-id="`)
			sb.WriteString(html.EscapeString(todo.TodoID))
			sb.WriteString(`" data-on:click="@post($api_base + '/api/v1/command', {headers: {Authorization: 'Bearer ' + $access_token}, payload: {action: 'delete-todo', title: '', group_id: $active_group_id, todo_id: evt.currentTarget.dataset.todoId}, filterSignals: {include: /^$/}})">Delete</button>`)
			sb.WriteString(`</div>`)
		} else {
			sb.WriteString(`<span class="badge badge-ghost badge-sm">read-only</span>`)
		}

		sb.WriteString(`</div></div></div>`)
	}

	sb.WriteString(`</div>`)
	return sb.String()
}

func renderEventsContainer(activeGroupID string) string {
	var sb strings.Builder
	sb.WriteString(`<div id="events" data-group-id="`)
	sb.WriteString(html.EscapeString(strings.TrimSpace(activeGroupID)))
	sb.WriteString(`" class="space-y-3 max-h-[24rem] overflow-auto pr-1"></div>`)
	return sb.String()
}
