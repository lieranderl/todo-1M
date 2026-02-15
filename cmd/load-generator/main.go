package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/todo-1m/project/internal/platform/metrics"
)

type config struct {
	CommandAPIBase          string
	SSEBase                 string
	Users                   int
	SetupConcurrency        int
	StartupWait             time.Duration
	Duration                time.Duration
	RampUp                  time.Duration
	ActionsPerUserPerSecond float64
	RequestTimeout          time.Duration
	MetricsAddr             string
	Password                string
	EnableSSE               bool
}

type authResponse struct {
	AccessToken string `json:"access_token"`
}

type groupResponse struct {
	ID string `json:"id"`
}

type commandResponse struct {
	TodoID string `json:"todo_id"`
}

type simulatedUser struct {
	Index       int
	Username    string
	Password    string
	ClientIP    string
	AccessToken string
	GroupID     string

	mu    sync.Mutex
	todos []string
}

type runner struct {
	cfg       config
	runID     string
	apiClient *http.Client
	sseClient *http.Client

	requestsSuccess atomic.Int64
	requestsError   atomic.Int64
	activeVUs       atomic.Int64
	activeSSE       atomic.Int64
}

var (
	requestsTotal = metrics.NewCounterVec(metrics.Opts{
		Name: "todo1m_loadgen_requests_total",
		Help: "Total HTTP requests sent by load generator.",
	}, []string{"endpoint", "method", "status", "outcome"})

	actionsTotal = metrics.NewCounterVec(metrics.Opts{
		Name: "todo1m_loadgen_actions_total",
		Help: "User actions executed by load generator.",
	}, []string{"action", "outcome"})

	virtualUsersGauge = metrics.NewGauge(metrics.Opts{
		Name: "todo1m_loadgen_virtual_users",
		Help: "Current number of active virtual users sending actions.",
	})

	sseConnectedUsersGauge = metrics.NewGauge(metrics.Opts{
		Name: "todo1m_loadgen_sse_connected_users",
		Help: "Current number of load-generated users with active SSE connections.",
	})
)

func init() {
	metrics.Default.MustRegister(requestsTotal, actionsTotal, virtualUsersGauge, sseConnectedUsersGauge)
}

func main() {
	cfg := loadConfig()
	if cfg.Users <= 0 {
		log.Fatal("LOADGEN_USERS must be > 0")
	}
	if cfg.SetupConcurrency <= 0 {
		log.Fatal("LOADGEN_SETUP_CONCURRENCY must be > 0")
	}

	baseCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx := baseCtx
	if cfg.Duration > 0 {
		timeoutCtx, cancel := context.WithTimeout(baseCtx, cfg.Duration)
		defer cancel()
		ctx = timeoutCtx
	}

	go runMetricsServer(cfg.MetricsAddr)

	transport := &http.Transport{
		MaxIdleConns:        cfg.Users * 4,
		MaxIdleConnsPerHost: cfg.Users * 4,
		IdleConnTimeout:     90 * time.Second,
	}

	r := &runner{
		cfg:   cfg,
		runID: strconv.FormatInt(time.Now().UTC().UnixNano(), 10),
		apiClient: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
		sseClient: &http.Client{
			Transport: transport,
		},
	}

	if err := r.waitForDependencies(ctx); err != nil {
		log.Fatalf("dependency readiness failed: %v", err)
	}

	users := r.setupUsers(ctx)
	if len(users) == 0 {
		log.Fatal("failed to initialize any users")
	}
	log.Printf("load generator initialized: users=%d duration=%s sse=%v rate_per_user=%.2f req/s",
		len(users), cfg.Duration.String(), cfg.EnableSSE, cfg.ActionsPerUserPerSecond)

	go r.logProgress(ctx)

	var wg sync.WaitGroup
	for idx := range users {
		user := users[idx]
		wg.Add(1)
		go func(u *simulatedUser) {
			defer wg.Done()
			r.runUser(ctx, u)
		}(user)
	}

	<-ctx.Done()
	wg.Wait()

	log.Printf("load test complete: success_requests=%d error_requests=%d",
		r.requestsSuccess.Load(), r.requestsError.Load())
}

func loadConfig() config {
	return config{
		CommandAPIBase:          trimRightSlash(stringEnv("LOADGEN_COMMAND_API_BASE", "http://command-api:8080")),
		SSEBase:                 trimRightSlash(stringEnv("LOADGEN_SSE_BASE", "http://sse-streamer:8081")),
		Users:                   intEnv("LOADGEN_USERS", 200),
		SetupConcurrency:        intEnv("LOADGEN_SETUP_CONCURRENCY", 25),
		StartupWait:             durationEnv("LOADGEN_STARTUP_WAIT", 2*time.Minute),
		Duration:                durationEnv("LOADGEN_DURATION", 10*time.Minute),
		RampUp:                  durationEnv("LOADGEN_RAMP_UP", 30*time.Second),
		ActionsPerUserPerSecond: floatEnv("LOADGEN_ACTIONS_PER_USER_PER_SECOND", 0.3),
		RequestTimeout:          durationEnv("LOADGEN_REQUEST_TIMEOUT", 10*time.Second),
		MetricsAddr:             stringEnv("LOADGEN_METRICS_ADDR", ":9099"),
		Password:                stringEnv("LOADGEN_PASSWORD", "load-test-pass-123"),
		EnableSSE:               boolEnv("LOADGEN_ENABLE_SSE", true),
	}
}

func (r *runner) waitForDependencies(ctx context.Context) error {
	wait := r.cfg.StartupWait
	if wait <= 0 {
		wait = 2 * time.Minute
	}

	if err := r.waitForHTTPStatus(ctx, r.cfg.CommandAPIBase+"/readyz", http.StatusOK, wait); err != nil {
		return fmt.Errorf("command-api not ready: %w", err)
	}
	if err := r.waitForHTTPStatus(ctx, r.cfg.SSEBase+"/readyz", http.StatusOK, wait); err != nil {
		return fmt.Errorf("sse-streamer not ready: %w", err)
	}
	return nil
}

func (r *runner) waitForHTTPStatus(ctx context.Context, requestURL string, expectedStatus int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			lastErr = err
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		resp, err := r.apiClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == expectedStatus {
			return nil
		}
		lastErr = fmt.Errorf("status=%d", resp.StatusCode)
		time.Sleep(1200 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timeout")
	}
	return lastErr
}

func (r *runner) setupUsers(ctx context.Context) []*simulatedUser {
	type setupResult struct {
		user *simulatedUser
		err  error
	}

	sem := make(chan struct{}, r.cfg.SetupConcurrency)
	results := make(chan setupResult, r.cfg.Users)
	var wg sync.WaitGroup

	for i := 0; i < r.cfg.Users; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			user, err := r.setupSingleUser(ctx, idx)
			results <- setupResult{user: user, err: err}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	users := make([]*simulatedUser, 0, r.cfg.Users)
	failures := 0
	for result := range results {
		if result.err != nil {
			failures++
			log.Printf("user setup failed: %v", result.err)
			continue
		}
		users = append(users, result.user)
	}
	log.Printf("user setup complete: success=%d failed=%d", len(users), failures)
	return users
}

func (r *runner) setupSingleUser(ctx context.Context, idx int) (*simulatedUser, error) {
	user := &simulatedUser{
		Index:    idx,
		Username: fmt.Sprintf("load-%s-%04d", r.runID, idx),
		Password: r.cfg.Password,
		ClientIP: fmt.Sprintf("10.0.%d.%d", 1+(idx/250), 1+(idx%250)),
	}

	var auth authResponse
	status, err := r.requestJSON(ctx, user, "register", http.MethodPost, r.cfg.CommandAPIBase+"/api/v1/auth/register", map[string]string{
		"username": user.Username,
		"password": user.Password,
	}, nil, &auth, http.StatusCreated, http.StatusConflict)
	if err != nil {
		return nil, fmt.Errorf("register %s: %w", user.Username, err)
	}

	if status == http.StatusConflict {
		auth = authResponse{}
		if _, err := r.requestJSON(ctx, user, "login", http.MethodPost, r.cfg.CommandAPIBase+"/api/v1/auth/login", map[string]string{
			"username": user.Username,
			"password": user.Password,
		}, nil, &auth, http.StatusOK); err != nil {
			return nil, fmt.Errorf("login %s: %w", user.Username, err)
		}
	}

	if strings.TrimSpace(auth.AccessToken) == "" {
		return nil, fmt.Errorf("empty access token for %s", user.Username)
	}
	user.AccessToken = auth.AccessToken

	var group groupResponse
	if _, err := r.requestJSON(ctx, user, "create_group", http.MethodPost, r.cfg.CommandAPIBase+"/api/v1/groups", map[string]string{
		"name": fmt.Sprintf("Load Group %d", user.Index),
	}, &user.AccessToken, &group, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("create group for %s: %w", user.Username, err)
	}
	if strings.TrimSpace(group.ID) == "" {
		return nil, fmt.Errorf("empty group id for %s", user.Username)
	}
	user.GroupID = group.ID

	return user, nil
}

func (r *runner) runUser(ctx context.Context, user *simulatedUser) {
	if r.cfg.RampUp > 0 {
		delay := time.Duration((float64(r.cfg.RampUp) / float64(maxInt(r.cfg.Users, 1))) * float64(user.Index))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}

	if r.cfg.EnableSSE {
		go r.runSSELoop(ctx, user)
	}

	virtualUsersGauge.Inc()
	r.activeVUs.Add(1)
	defer virtualUsersGauge.Dec()
	defer r.activeVUs.Add(-1)

	interval := time.Second
	if r.cfg.ActionsPerUserPerSecond > 0 {
		interval = time.Duration(float64(time.Second) / r.cfg.ActionsPerUserPerSecond)
		if interval < 25*time.Millisecond {
			interval = 25 * time.Millisecond
		}
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(user.Index*7)))
	initialJitter := time.Duration(rng.Int63n(int64(interval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialJitter):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runAction(ctx, user, rng)
		}
	}
}

func (r *runner) runAction(ctx context.Context, user *simulatedUser, rng *rand.Rand) {
	todoID, hasTodo := user.randomTodo(rng)

	choice := rng.Float64()
	switch {
	case !hasTodo || choice < 0.60:
		r.createTodo(ctx, user, rng)
	case choice < 0.90:
		r.updateTodo(ctx, user, rng, todoID)
	default:
		r.deleteTodo(ctx, user, todoID)
	}
}

func (r *runner) createTodo(ctx context.Context, user *simulatedUser, rng *rand.Rand) {
	var resp commandResponse
	_, err := r.requestJSON(ctx, user, "command_create", http.MethodPost, r.cfg.CommandAPIBase+"/api/v1/command", map[string]string{
		"action":   "create-todo",
		"group_id": user.GroupID,
		"title":    fmt.Sprintf("Load Todo %d", rng.Intn(1_000_000)),
	}, &user.AccessToken, &resp, http.StatusAccepted)
	if err != nil {
		actionsTotal.WithLabelValues("create", "error").Inc()
		return
	}
	if strings.TrimSpace(resp.TodoID) != "" {
		user.addTodo(resp.TodoID)
	}
	actionsTotal.WithLabelValues("create", "success").Inc()
}

func (r *runner) updateTodo(ctx context.Context, user *simulatedUser, rng *rand.Rand, todoID string) {
	if strings.TrimSpace(todoID) == "" {
		r.createTodo(ctx, user, rng)
		return
	}

	_, err := r.requestJSON(ctx, user, "command_update", http.MethodPost, r.cfg.CommandAPIBase+"/api/v1/command", map[string]string{
		"action":   "update-todo",
		"group_id": user.GroupID,
		"todo_id":  todoID,
		"title":    fmt.Sprintf("Updated Load Todo %d", rng.Intn(1_000_000)),
	}, &user.AccessToken, nil, http.StatusAccepted)
	if err != nil {
		actionsTotal.WithLabelValues("update", "error").Inc()
		return
	}
	actionsTotal.WithLabelValues("update", "success").Inc()
}

func (r *runner) deleteTodo(ctx context.Context, user *simulatedUser, todoID string) {
	if strings.TrimSpace(todoID) == "" {
		actionsTotal.WithLabelValues("delete", "error").Inc()
		return
	}

	_, err := r.requestJSON(ctx, user, "command_delete", http.MethodPost, r.cfg.CommandAPIBase+"/api/v1/command", map[string]string{
		"action":   "delete-todo",
		"group_id": user.GroupID,
		"todo_id":  todoID,
		"title":    "",
	}, &user.AccessToken, nil, http.StatusAccepted)
	if err != nil {
		actionsTotal.WithLabelValues("delete", "error").Inc()
		return
	}
	user.removeTodo(todoID)
	actionsTotal.WithLabelValues("delete", "success").Inc()
}

func (r *runner) runSSELoop(ctx context.Context, user *simulatedUser) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := r.connectAndReadSSE(ctx, user)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("sse reconnect user=%s err=%v", user.Username, err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(1200 * time.Millisecond):
		}
	}
}

func (r *runner) connectAndReadSSE(ctx context.Context, user *simulatedUser) error {
	sseURL := r.cfg.SSEBase + "/events?group_id=" + url.QueryEscape(user.GroupID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Forwarded-For", user.ClientIP)

	resp, err := r.sseClient.Do(req)
	if err != nil {
		requestsTotal.WithLabelValues("events_stream_open", http.MethodGet, "0", "error").Inc()
		r.requestsError.Add(1)
		return err
	}
	defer resp.Body.Close()

	statusText := strconv.Itoa(resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		requestsTotal.WithLabelValues("events_stream_open", http.MethodGet, statusText, "error").Inc()
		r.requestsError.Add(1)
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("unexpected SSE status: %d", resp.StatusCode)
	}

	requestsTotal.WithLabelValues("events_stream_open", http.MethodGet, statusText, "success").Inc()
	r.requestsSuccess.Add(1)

	sseConnectedUsersGauge.Inc()
	r.activeSSE.Add(1)
	defer sseConnectedUsersGauge.Dec()
	defer r.activeSSE.Add(-1)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return context.Canceled
		}
		return err
	}
	return nil
}

func (r *runner) requestJSON(
	ctx context.Context,
	user *simulatedUser,
	endpoint, method, requestURL string,
	payload any,
	bearerToken *string,
	out any,
	expectedStatuses ...int,
) (int, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Forwarded-For", user.ClientIP)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearerToken != nil && strings.TrimSpace(*bearerToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*bearerToken))
	}

	resp, err := r.apiClient.Do(req)
	if err != nil {
		requestsTotal.WithLabelValues(endpoint, method, "0", "error").Inc()
		r.requestsError.Add(1)
		return 0, err
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		requestsTotal.WithLabelValues(endpoint, method, strconv.Itoa(resp.StatusCode), "error").Inc()
		r.requestsError.Add(1)
		return resp.StatusCode, readErr
	}

	statusText := strconv.Itoa(resp.StatusCode)
	if isExpectedStatus(resp.StatusCode, expectedStatuses) {
		requestsTotal.WithLabelValues(endpoint, method, statusText, "success").Inc()
		r.requestsSuccess.Add(1)
		if out != nil && len(responseBody) > 0 {
			if err := json.Unmarshal(responseBody, out); err != nil {
				return resp.StatusCode, err
			}
		}
		return resp.StatusCode, nil
	}

	requestsTotal.WithLabelValues(endpoint, method, statusText, "error").Inc()
	r.requestsError.Add(1)
	return resp.StatusCode, fmt.Errorf("unexpected status=%d body=%s", resp.StatusCode, truncate(string(responseBody), 240))
}

func (r *runner) logProgress(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("progress: success_requests=%d error_requests=%d active_vus=%d active_sse=%d",
				r.requestsSuccess.Load(),
				r.requestsError.Load(),
				r.activeVUs.Load(),
				r.activeSSE.Load(),
			)
		}
	}
}

func runMetricsServer(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.DefaultHandler())
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("load generator metrics endpoint listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("load generator metrics server failed: %v", err)
	}
}

func (u *simulatedUser) addTodo(todoID string) {
	if strings.TrimSpace(todoID) == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.todos = append(u.todos, todoID)
}

func (u *simulatedUser) randomTodo(rng *rand.Rand) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.todos) == 0 {
		return "", false
	}
	return u.todos[rng.Intn(len(u.todos))], true
}

func (u *simulatedUser) removeTodo(todoID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for idx, existing := range u.todos {
		if existing != todoID {
			continue
		}
		u.todos[idx] = u.todos[len(u.todos)-1]
		u.todos = u.todos[:len(u.todos)-1]
		return
	}
}

func trimRightSlash(v string) string {
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

func isExpectedStatus(status int, expected []int) bool {
	for _, candidate := range expected {
		if status == candidate {
			return true
		}
	}
	return false
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func stringEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func intEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func floatEnv(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func boolEnv(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}
