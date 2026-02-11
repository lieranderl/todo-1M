//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type managedProcess struct {
	name   string
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}

	mu      sync.RWMutex
	exited  bool
	exitErr error
}

type localStack struct {
	root        string
	commandURL  string
	streamURL   string
	databaseURL string

	domain   *managedProcess
	sink     *managedProcess
	command  *managedProcess
	streamer *managedProcess
}

type sseStream struct {
	resp   *http.Response
	cancel context.CancelFunc
	lines  chan string
	errs   chan error
}

var (
	buildOnce sync.Once
	buildErr  error
)

func TestCommandToEventToPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	token, groupID := bootstrapUserAndGroup(t, stack.commandURL)

	title := fmt.Sprintf("integration-todo-%d", time.Now().UnixNano())
	status, body := postCommand(t, stack.commandURL, token, groupID, title)
	if status != http.StatusAccepted {
		t.Fatalf("unexpected response status=%d body=%s", status, body)
	}

	var resp struct {
		Status    string `json:"status"`
		CommandID string `json:"command_id"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v body=%s", err, body)
	}
	if resp.Status != "accepted" || resp.CommandID == "" {
		t.Fatalf("unexpected response payload: %+v", resp)
	}

	waitForPersistedRow(t, stack.databaseURL, groupID, title, 30*time.Second, stack.processes()...)
}

func TestSSEStreamReceivesTodoPatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	token, groupID := bootstrapUserAndGroup(t, stack.commandURL)
	stream := openSSEStream(t, stack.streamURL+"?group_id="+groupID+"&token="+token)
	t.Cleanup(func() { stream.Close() })

	waitForLineContains(t, stream, "Connected to Group Stream!", 10*time.Second)

	title := fmt.Sprintf("integration-stream-%d", time.Now().UnixNano())
	status, body := postCommand(t, stack.commandURL, token, groupID, title)
	if status != http.StatusAccepted {
		t.Fatalf("unexpected response status=%d body=%s", status, body)
	}

	waitForLineContains(t, stream, "event: datastar-patch-elements", 10*time.Second)
	waitForLineContains(t, stream, title, 10*time.Second)
}

func TestGroupMembersShareVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	ownerToken, groupID := bootstrapUserAndGroup(t, stack.commandURL)
	memberToken, memberUsername := registerUserAndGetToken(t, stack.commandURL, "member")
	addMemberToGroup(t, stack.commandURL, ownerToken, groupID, memberUsername)

	memberStream := openSSEStream(t, stack.streamURL+"?group_id="+groupID+"&token="+memberToken)
	t.Cleanup(func() { memberStream.Close() })
	waitForLineContains(t, memberStream, "Connected to Group Stream!", 10*time.Second)

	title := fmt.Sprintf("integration-shared-%d", time.Now().UnixNano())
	status, body := postCommand(t, stack.commandURL, ownerToken, groupID, title)
	if status != http.StatusAccepted {
		t.Fatalf("unexpected response status=%d body=%s", status, body)
	}

	waitForLineContains(t, memberStream, title, 10*time.Second)
}

func TestTodoEditDeleteProjectionAndActorInfo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	token, groupID := bootstrapUserAndGroup(t, stack.commandURL)

	createTitle := fmt.Sprintf("integration-editable-%d", time.Now().UnixNano())
	status, body := postCommandBody(t, stack.commandURL, token, map[string]any{
		"action":   "create-todo",
		"group_id": groupID,
		"title":    createTitle,
	})
	if status != http.StatusAccepted {
		t.Fatalf("create command failed status=%d body=%s", status, body)
	}

	var createResp struct {
		Status    string `json:"status"`
		CommandID string `json:"command_id"`
		TodoID    string `json:"todo_id"`
	}
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("invalid create response JSON: %v body=%s", err, body)
	}
	if createResp.Status != "accepted" || createResp.TodoID == "" {
		t.Fatalf("unexpected create response: %+v", createResp)
	}

	waitForTodoTitle(t, stack.streamURL, token, groupID, createResp.TodoID, createTitle, 10*time.Second)

	todos := getTodos(t, stack.streamURL, token, groupID)
	foundCreated := false
	for _, todo := range todos {
		if todo.TodoID != createResp.TodoID {
			continue
		}
		foundCreated = true
		if todo.CreatedByUsername == "" || todo.UpdatedByUsername == "" {
			t.Fatalf("expected actor metadata in todo: %+v", todo)
		}
		break
	}
	if !foundCreated {
		t.Fatalf("created todo %s not found in query model", createResp.TodoID)
	}

	updatedTitle := createTitle + "-updated"
	status, body = postCommandBody(t, stack.commandURL, token, map[string]any{
		"action":   "update-todo",
		"group_id": groupID,
		"todo_id":  createResp.TodoID,
		"title":    updatedTitle,
	})
	if status != http.StatusAccepted {
		t.Fatalf("update command failed status=%d body=%s", status, body)
	}

	waitForTodoTitle(t, stack.streamURL, token, groupID, createResp.TodoID, updatedTitle, 10*time.Second)

	status, body = postCommandBody(t, stack.commandURL, token, map[string]any{
		"action":   "delete-todo",
		"group_id": groupID,
		"todo_id":  createResp.TodoID,
	})
	if status != http.StatusAccepted {
		t.Fatalf("delete command failed status=%d body=%s", status, body)
	}

	waitForTodoAbsent(t, stack.streamURL, token, groupID, createResp.TodoID, 10*time.Second)
}

func TestWorkspacePageBootstrapsWithOneShotEffect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	page := getAppPage(t, stack.streamURL)

	if !strings.Contains(page, "window.location.origin.replace(':8081', ':8080').replace(':18081', ':18080')") {
		t.Fatalf("workspace page missing dynamic api_base signal")
	}
	if !strings.Contains(page, "workspace_bootstrapped: false") {
		t.Fatalf("workspace page missing workspace_bootstrapped signal")
	}
	if !strings.Contains(page, `data-effect="$access_token && !$workspace_bootstrapped && (`) {
		t.Fatalf("workspace page missing one-shot data-effect bootstrap for group loading")
	}
	if !strings.Contains(page, `@get('/ui/workspace?group_id=' + $active_group_id`) {
		t.Fatalf("workspace page bootstrap missing workspace GET action")
	}
	if !strings.Contains(page, `@setAll(true, {include: /^workspace_bootstrapped$/})`) {
		t.Fatalf("workspace page bootstrap missing workspace_bootstrapped setAll")
	}
	if !strings.Contains(page, `@get('/ui/workspace?group_id=' + $active_group_id`) ||
		!strings.Contains(page, `@setAll(true, {include: /^workspace_bootstrapped$/})`) {
		t.Fatalf("workspace page bootstrap should use comma-operator action chain with fetch-before-flag order")
	}
	if strings.Index(page, `@setAll(true, {include: /^workspace_bootstrapped$/})`) < strings.Index(page, `@get('/ui/workspace?group_id=' + $active_group_id`) {
		t.Fatalf("workspace page bootstrap sets workspace_bootstrapped before fetching workspace")
	}
	if strings.Contains(page, `$access_token && !$workspace_bootstrapped && (@get('/ui/workspace?group_id=' + $active_group_id, {headers: {Authorization: 'Bearer ' + $access_token}, filterSignals: {include: /^$/}}), ($active_group_id && @get('/events?group_id=' + $active_group_id + '&token=' + $access_token`) {
		t.Fatalf("workspace bootstrap should not auto-open events stream before explicit group connect/select")
	}
	if strings.Contains(page, "@setAll(true, {include: /^workspace_bootstrapped$/}); @get('/ui/workspace?group_id=' + $active_group_id") {
		t.Fatalf("workspace page bootstrap still uses invalid semicolon action chain inside parenthesized expression")
	}
	if strings.Contains(page, `data-init="$access_token && @get('/ui/workspace?group_id=' + $active_group_id`) {
		t.Fatalf("workspace page still uses data-init bootstrap instead of one-shot data-effect")
	}
	if strings.Contains(page, `data-on:load="$access_token && @get('/ui/workspace?group_id=' + $active_group_id`) {
		t.Fatalf("workspace page still uses data-on:load bootstrap instead of one-shot data-effect")
	}
}

func TestWorkspaceFragmentUsesGroupNamesAndSelectionBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	token, activeGroupID := bootstrapUserAndGroup(t, stack.commandURL)
	secondGroupName := fmt.Sprintf("name-check-%d", time.Now().UnixNano())
	secondGroupID := createGroup(t, stack.commandURL, token, secondGroupName)
	if secondGroupID == "" {
		t.Fatal("expected non-empty second group id")
	}

	fragment := getWorkspaceFragment(t, stack.streamURL, token, activeGroupID)

	if !strings.Contains(fragment, secondGroupName+" [owner]") {
		t.Fatalf("workspace groups list does not render group name label: %q", secondGroupName)
	}
	if !strings.Contains(fragment, `data-group-id="`+secondGroupID+`"`) {
		t.Fatalf("workspace groups list missing group id dataset binding for %q", secondGroupID)
	}
	if !strings.Contains(fragment, `@setAll(evt.currentTarget.dataset.groupId, {include: /^active_group_id$/})`) {
		t.Fatalf("workspace groups list missing evt.currentTarget-based active_group_id assignment")
	}
	if strings.Contains(fragment, "this.dataset.groupId") {
		t.Fatalf("workspace groups list still uses unsupported this.dataset.groupId")
	}
	if !strings.Contains(fragment, `@get('/ui/workspace?group_id=' + evt.currentTarget.dataset.groupId`) {
		t.Fatalf("workspace groups list missing selected-group fetch expression")
	}
}

func TestWorkspaceFragmentUsesSafeTodoButtonBindings(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	token, groupID := bootstrapUserAndGroup(t, stack.commandURL)

	title := fmt.Sprintf("todo-bindings-%d", time.Now().UnixNano())
	status, body := postCommand(t, stack.commandURL, token, groupID, title)
	if status != http.StatusAccepted {
		t.Fatalf("unexpected response status=%d body=%s", status, body)
	}
	waitForPersistedRow(t, stack.databaseURL, groupID, title, 20*time.Second, stack.processes()...)

	fragment := getWorkspaceFragment(t, stack.streamURL, token, groupID)
	if !strings.Contains(fragment, title) {
		t.Fatalf("workspace todos fragment missing created todo title %q", title)
	}
	if !strings.Contains(fragment, "evt.currentTarget.dataset.inputId") {
		t.Fatalf("workspace todo update action missing evt.currentTarget.dataset.inputId")
	}
	if !strings.Contains(fragment, "evt.currentTarget.dataset.todoId") {
		t.Fatalf("workspace todo actions missing evt.currentTarget.dataset.todoId")
	}
	if strings.Contains(fragment, "this.dataset.inputId") || strings.Contains(fragment, "this.dataset.todoId") {
		t.Fatalf("workspace todo actions still use unsupported this.dataset bindings")
	}
}

func TestDeleteGroupOwnerOnlyAndWorkspaceRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	stack := startLocalStack(t)
	ownerToken, ownerGroupID := bootstrapUserAndGroup(t, stack.commandURL)
	memberToken, memberUsername := registerUserAndGetToken(t, stack.commandURL, "member")
	addMemberToGroup(t, stack.commandURL, ownerToken, ownerGroupID, memberUsername)

	status, body := deleteGroup(t, stack.commandURL, memberToken, ownerGroupID)
	if status != http.StatusForbidden {
		t.Fatalf("expected member delete to be forbidden status=%d body=%s", status, body)
	}

	deleteGroupName := fmt.Sprintf("delete-me-%d", time.Now().UnixNano())
	deleteGroupID := createGroup(t, stack.commandURL, ownerToken, deleteGroupName)
	if deleteGroupID == "" {
		t.Fatalf("expected non-empty group id for deletable group")
	}

	before := getWorkspaceFragment(t, stack.streamURL, ownerToken, ownerGroupID)
	if !strings.Contains(before, deleteGroupName+" [owner]") {
		t.Fatalf("expected workspace fragment to include deletable group name %q", deleteGroupName)
	}
	if !strings.Contains(before, "@delete($api_base + '/api/v1/groups/' + evt.currentTarget.dataset.groupId") {
		t.Fatalf("expected workspace fragment to include delete group datastar action")
	}

	status, body = deleteGroup(t, stack.commandURL, ownerToken, deleteGroupID)
	if status != http.StatusNoContent {
		t.Fatalf("owner delete failed status=%d body=%s", status, body)
	}

	after := getWorkspaceFragment(t, stack.streamURL, ownerToken, ownerGroupID)
	if strings.Contains(after, deleteGroupName+" [owner]") {
		t.Fatalf("workspace fragment still contains deleted group name %q", deleteGroupName)
	}
	groups := listGroups(t, strings.TrimSuffix(stack.streamURL, "/events"), ownerToken)
	for _, g := range groups {
		if g.GroupID == deleteGroupID {
			t.Fatalf("deleted group %q still present in list groups response", deleteGroupID)
		}
	}
}

func startLocalStack(t *testing.T) *localStack {
	t.Helper()

	root := repoRoot(t)
	if !dockerAvailable(root) {
		t.Skip("docker compose is not available in PATH")
	}

	runCommand(t, root, "docker", "compose", "up", "-d")
	t.Cleanup(func() {
		cmd := exec.Command("docker", "compose", "down")
		cmd.Dir = root
		_ = cmd.Run()
	})

	waitForTCP(t, "127.0.0.1:4222", 30*time.Second)
	waitForTCP(t, "127.0.0.1:5432", 30*time.Second)
	buildServices(t, root)

	stack := &localStack{
		root:        root,
		commandURL:  "http://127.0.0.1:18080/api/v1/command",
		streamURL:   "http://127.0.0.1:18081/events",
		databaseURL: "postgres://app:password@localhost:5432/app?sslmode=disable",
	}

	stack.domain = startProcess(t, root, "domain-engine", nil, "./bin/domain-engine")
	stack.sink = startProcess(t, root, "data-sink", []string{"DATABASE_URL=" + stack.databaseURL}, "./bin/data-sink")
	stack.command = startProcess(t, root, "command-api", []string{
		"COMMAND_API_ADDR=:18080",
		"UI_ORIGIN=http://localhost:18081",
		"DATABASE_URL=" + stack.databaseURL,
		"JWT_SECRET=integration-secret",
	}, "./bin/command-api")
	stack.streamer = startProcess(t, root, "sse-streamer", []string{
		"SSE_STREAMER_ADDR=:18081",
		"DATABASE_URL=" + stack.databaseURL,
		"JWT_SECRET=integration-secret",
	}, "./bin/sse-streamer")

	t.Cleanup(func() {
		stopProcess(stack.streamer)
		stopProcess(stack.command)
		stopProcess(stack.sink)
		stopProcess(stack.domain)
	})

	requireProcessesAlive(t, stack.processes()...)
	waitForTCP(t, "127.0.0.1:18080", 30*time.Second, stack.processes()...)
	waitForTCP(t, "127.0.0.1:18081", 30*time.Second, stack.processes()...)
	waitForTable(t, stack.databaseURL, "todo_events", 30*time.Second, stack.processes()...)
	return stack
}

func (s *localStack) processes() []*managedProcess {
	return []*managedProcess{s.domain, s.sink, s.command, s.streamer}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repository root from %s", dir)
		}
		dir = parent
	}
}

func dockerAvailable(root string) bool {
	cmd := exec.Command("docker", "compose", "version")
	cmd.Dir = root
	return cmd.Run() == nil
}

func buildServices(t *testing.T, root string) {
	t.Helper()
	buildOnce.Do(func() {
		builds := []struct {
			out string
			pkg string
		}{
			{"bin/command-api", "./cmd/command-api"},
			{"bin/domain-engine", "./cmd/domain-engine"},
			{"bin/data-sink", "./cmd/data-sink"},
			{"bin/sse-streamer", "./cmd/sse-streamer"},
		}
		for _, b := range builds {
			if err := runCommandErr(root, "go", "build", "-o", b.out, b.pkg); err != nil {
				buildErr = err
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("build services failed: %v", buildErr)
	}
}

func runCommand(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	if err := runCommandErr(dir, name, args...); err != nil {
		t.Fatalf("%v", err)
	}
}

func runCommandErr(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("command failed: %s %v\nerror: %v\noutput:\n%s", name, args, err, string(output))
	}
	return nil
}

func startProcess(t *testing.T, dir string, name string, env []string, command string, args ...string) *managedProcess {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	p := &managedProcess{
		name: name,
		cmd:  cmd,
		done: make(chan struct{}),
	}
	cmd.Stdout = &p.stdout
	cmd.Stderr = &p.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start %s: %v", name, err)
	}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exited = true
		p.exitErr = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p
}

func stopProcess(p *managedProcess) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}

	select {
	case <-p.done:
		return
	default:
	}

	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		return
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func waitForTCP(t *testing.T, addr string, timeout time.Duration, processes ...*managedProcess) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(processes) > 0 {
			requireProcessesAlive(t, processes...)
		}

		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(processes) > 0 {
		t.Fatalf("timeout waiting for tcp service at %s\n%s", addr, processDebug(processes...))
	}
	t.Fatalf("timeout waiting for tcp service at %s", addr)
}

func waitForTable(t *testing.T, databaseURL string, table string, timeout time.Duration, processes ...*managedProcess) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		requireProcessesAlive(t, processes...)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			var got *string
			queryErr := pool.QueryRow(ctx, "select to_regclass($1)", "public."+table).Scan(&got)
			pool.Close()
			cancel()
			if queryErr == nil && got != nil && (*got == table || *got == "public."+table) {
				return
			}
		} else {
			cancel()
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for table %s\n%s", table, processDebug(processes...))
}

func postCommand(t *testing.T, commandURL string, token string, groupID string, title string) (int, string) {
	t.Helper()
	return postCommandBody(t, commandURL, token, map[string]any{
		"action":   "create-todo",
		"group_id": groupID,
		"title":    title,
	})
}

func deleteGroup(t *testing.T, commandURL string, token string, groupID string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodDelete,
		strings.TrimSuffix(commandURL, "/api/v1/command")+"/api/v1/groups/"+url.PathEscape(groupID),
		nil,
	)
	if err != nil {
		t.Fatalf("create delete group request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("delete group failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read delete group body failed: %v", err)
	}
	return resp.StatusCode, body
}

func listGroups(t *testing.T, streamerBaseURL, token string) []struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	Role      string `json:"role"`
} {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodGet,
		strings.TrimSuffix(streamerBaseURL, "/")+"/api/v1/groups",
		nil,
	)
	if err != nil {
		t.Fatalf("create list groups request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("list groups failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read list groups body failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list groups failed status=%d body=%s", resp.StatusCode, body)
	}

	var parsed struct {
		Groups []struct {
			GroupID   string `json:"group_id"`
			GroupName string `json:"group_name"`
			Role      string `json:"role"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("invalid list groups JSON: %v body=%s", err, body)
	}
	return parsed.Groups
}

func createGroup(t *testing.T, commandURL string, token string, name string) string {
	t.Helper()

	reqBytes, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatalf("marshal create group payload failed: %v", err)
	}
	req, err := http.NewRequest(
		http.MethodPost,
		strings.TrimSuffix(commandURL, "/api/v1/command")+"/api/v1/groups",
		bytes.NewBuffer(reqBytes),
	)
	if err != nil {
		t.Fatalf("create group request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create group failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read create group body failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group failed status=%d body=%s", resp.StatusCode, body)
	}

	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("invalid create group JSON: %v body=%s", err, body)
	}
	return parsed.ID
}

func postCommandBody(t *testing.T, commandURL string, token string, payload map[string]any) (int, string) {
	t.Helper()
	reqBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal command payload failed: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, commandURL, bytes.NewBuffer(reqBytes))
	if err != nil {
		t.Fatalf("create request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post command failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body failed: %v", err)
	}
	return resp.StatusCode, body
}

type todoQueryResponse struct {
	Todos []struct {
		TodoID            string `json:"todo_id"`
		GroupID           string `json:"group_id"`
		Title             string `json:"title"`
		CreatedByUserID   string `json:"created_by_user_id"`
		CreatedByUsername string `json:"created_by_username"`
		UpdatedByUserID   string `json:"updated_by_user_id"`
		UpdatedByUsername string `json:"updated_by_username"`
	} `json:"todos"`
}

func getTodos(t *testing.T, streamURL, token, groupID string) []struct {
	TodoID            string `json:"todo_id"`
	GroupID           string `json:"group_id"`
	Title             string `json:"title"`
	CreatedByUserID   string `json:"created_by_user_id"`
	CreatedByUsername string `json:"created_by_username"`
	UpdatedByUserID   string `json:"updated_by_user_id"`
	UpdatedByUsername string `json:"updated_by_username"`
} {
	t.Helper()
	url := strings.TrimSuffix(streamURL, "/events") + "/api/v1/todos?group_id=" + groupID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("create get todos request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get todos failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read get todos body failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get todos failed status=%d body=%s", resp.StatusCode, body)
	}

	var parsed todoQueryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("invalid get todos JSON: %v body=%s", err, body)
	}
	return parsed.Todos
}

func getWorkspaceFragment(t *testing.T, streamURL, token, groupID string) string {
	t.Helper()

	base := strings.TrimSuffix(streamURL, "/events")
	fragmentURL := base + "/ui/workspace?group_id=" + url.QueryEscape(groupID)
	req, err := http.NewRequest(http.MethodGet, fragmentURL, nil)
	if err != nil {
		t.Fatalf("create workspace fragment request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get workspace fragment failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read workspace fragment body failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workspace fragment failed status=%d body=%s", resp.StatusCode, body)
	}
	return body
}

func getAppPage(t *testing.T, streamURL string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(streamURL, "/events")+"/app", nil)
	if err != nil {
		t.Fatalf("create app page request failed: %v", err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get app page failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read app page body failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app page failed status=%d body=%s", resp.StatusCode, body)
	}
	return body
}

func waitForTodoTitle(t *testing.T, streamURL, token, groupID, todoID, title string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		todos := getTodos(t, streamURL, token, groupID)
		for _, todo := range todos {
			if todo.TodoID == todoID && todo.Title == title {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for todo %s title=%q", todoID, title)
}

func waitForTodoAbsent(t *testing.T, streamURL, token, groupID, todoID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		todos := getTodos(t, streamURL, token, groupID)
		found := false
		for _, todo := range todos {
			if todo.TodoID == todoID {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for todo %s to be absent", todoID)
}

func waitForPersistedRow(t *testing.T, databaseURL string, groupID string, title string, timeout time.Duration, processes ...*managedProcess) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		requireProcessesAlive(t, processes...)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			var count int
			queryErr := pool.QueryRow(ctx,
				"select count(*) from todo_events where group_id=$1 and title=$2",
				groupID,
				title,
			).Scan(&count)
			pool.Close()
			cancel()
			if queryErr == nil && count > 0 {
				return
			}
		} else {
			cancel()
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for persisted row title=%q\n%s", title, processDebug(processes...))
}

func bootstrapUserAndGroup(t *testing.T, commandURL string) (token string, groupID string) {
	t.Helper()
	username := fmt.Sprintf("owner_%d", time.Now().UnixNano())
	password := "password123"

	registerStatus, registerBody := authRequest(t, commandURL, "/api/v1/auth/register", username, password)
	if registerStatus != http.StatusCreated {
		t.Fatalf("register failed status=%d body=%s", registerStatus, registerBody)
	}
	var reg struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(registerBody), &reg); err != nil {
		t.Fatalf("invalid register JSON: %v body=%s", err, registerBody)
	}
	if reg.Token == "" {
		t.Fatalf("register returned empty token: %s", registerBody)
	}

	createGroupReq := `{"name":"integration-group"}`
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(commandURL, "/api/v1/command")+"/api/v1/groups", bytes.NewBufferString(createGroupReq))
	if err != nil {
		t.Fatalf("create group request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create group failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read create group response failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group failed status=%d body=%s", resp.StatusCode, body)
	}
	var group struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &group); err != nil {
		t.Fatalf("invalid create group JSON: %v body=%s", err, body)
	}
	if group.ID == "" {
		t.Fatalf("create group returned empty id: %s", body)
	}
	return reg.Token, group.ID
}

func registerUserAndGetToken(t *testing.T, commandURL string, usernamePrefix string) (token string, username string) {
	t.Helper()
	username = fmt.Sprintf("%s_%d", usernamePrefix, time.Now().UnixNano())
	status, body := authRequest(t, commandURL, "/api/v1/auth/register", username, "password123")
	if status != http.StatusCreated {
		t.Fatalf("register failed status=%d body=%s", status, body)
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("invalid register JSON: %v body=%s", err, body)
	}
	if resp.Token == "" {
		t.Fatalf("empty token in response: %s", body)
	}
	return resp.Token, username
}

func addMemberToGroup(t *testing.T, commandURL string, ownerToken, groupID, username string) {
	t.Helper()
	reqBody := fmt.Sprintf(`{"username":"%s"}`, username)
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(commandURL, "/api/v1/command")+"/api/v1/groups/"+groupID+"/members", bytes.NewBufferString(reqBody))
	if err != nil {
		t.Fatalf("create add-member request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("add member failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := ioReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("add member failed status=%d body=%s", resp.StatusCode, body)
	}
}

func authRequest(t *testing.T, commandURL string, path string, username string, password string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"username":"%s","password":"%s"}`, username, password)
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(commandURL, "/api/v1/command")+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("create auth request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("auth request failed: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := ioReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read auth response failed: %v", err)
	}
	return resp.StatusCode, respBody
}

func openSSEStream(t *testing.T, streamURL string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("create SSE request failed: %v", err)
	}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open SSE stream failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := ioReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("unexpected SSE status=%d body=%s", resp.StatusCode, body)
	}

	stream := &sseStream{
		resp:   resp,
		cancel: cancel,
		lines:  make(chan string, 512),
		errs:   make(chan error, 1),
	}

	go func() {
		defer close(stream.lines)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 1024), 1024*1024)
		for scanner.Scan() {
			stream.lines <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			stream.errs <- err
			return
		}
		stream.errs <- io.EOF
	}()

	return stream
}

func (s *sseStream) Close() {
	if s == nil {
		return
	}
	s.cancel()
	_ = s.resp.Body.Close()
}

func waitForLineContains(t *testing.T, stream *sseStream, needle string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	var recent []string
	for {
		select {
		case line, ok := <-stream.lines:
			if !ok {
				select {
				case err := <-stream.errs:
					t.Fatalf("SSE stream closed before matching %q: %v\nrecent lines:\n%s", needle, err, strings.Join(recent, "\n"))
				default:
					t.Fatalf("SSE stream closed before matching %q\nrecent lines:\n%s", needle, strings.Join(recent, "\n"))
				}
			}
			if len(recent) >= 20 {
				recent = recent[1:]
			}
			recent = append(recent, line)
			if strings.Contains(line, needle) {
				return line
			}
		case err := <-stream.errs:
			t.Fatalf("SSE stream error before matching %q: %v\nrecent lines:\n%s", needle, err, strings.Join(recent, "\n"))
		case <-deadline:
			t.Fatalf("timeout waiting for SSE line containing %q\nrecent lines:\n%s", needle, strings.Join(recent, "\n"))
		}
	}
}

func ioReadAll(r io.Reader) (string, error) {
	buf := new(bytes.Buffer)
	_, err := io.Copy(buf, r)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (p *managedProcess) debugString() string {
	return fmt.Sprintf("[%s]\nstdout:\n%s\nstderr:\n%s\n", p.name, p.stdout.String(), p.stderr.String())
}

func (p *managedProcess) state() (bool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.exited, p.exitErr
}

func requireProcessesAlive(t *testing.T, processes ...*managedProcess) {
	t.Helper()
	for _, p := range processes {
		exited, err := p.state()
		if exited {
			if err == nil {
				t.Fatalf("%s exited unexpectedly.\n%s", p.name, p.debugString())
			}
			t.Fatalf("%s failed: %v\n%s", p.name, err, p.debugString())
		}
	}
}

func processDebug(processes ...*managedProcess) string {
	var out []string
	for _, p := range processes {
		out = append(out, p.debugString())
	}
	return strings.Join(out, "\n")
}
