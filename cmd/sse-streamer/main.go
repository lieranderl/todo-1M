package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

		sendFragment := func(msg string, subtitle string) {
			component := frontend.EventItem(msg, subtitle)
			var buf bytes.Buffer
			if err := component.Render(r.Context(), &buf); err != nil {
				return
			}
			content := strings.ReplaceAll(buf.String(), "\n", "")
			fmt.Fprint(w, "event: datastar-patch-elements\n")
			fmt.Fprint(w, "data: selector #events\n")
			fmt.Fprint(w, "data: mode prepend\n")
			fmt.Fprintf(w, "data: elements %s\n\n", content)
			flusher.Flush()
		}

		type todoEventPayload struct {
			contracts.TodoEvent
			EventSeq uint64 `json:"event_seq"`
		}

		sendTodoEvent := func(event contracts.TodoEvent, eventSeq uint64) {
			payload, err := json.Marshal(todoEventPayload{
				TodoEvent: event,
				EventSeq:  eventSeq,
			})
			if err != nil {
				return
			}
			fmt.Fprint(w, "event: todo-event\n")
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(payload), "\n", ""))
			flusher.Flush()
		}

		sendFragment("Connected to Group Stream!", "Waiting for updates...")

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
				sendTodoEvent(event, streamEvent.Seq)
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
