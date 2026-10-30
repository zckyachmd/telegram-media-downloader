package mcpcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Reader.Close()
		_ = db.Writer.Close()
	})
	key := []byte(strings.Repeat("k", 32))
	s, err := New(db.Writer, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func createTestAgent(t *testing.T, s *Store) Agent {
	t.Helper()
	agent, _, err := s.CreateAgent(context.Background(), AgentInput{Name: "test agent", Role: "custom", Scopes: []string{"queue.read"}})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestAgentTokenIsScopedAndRevokedOnUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, secret, err := s.CreateAgent(ctx, AgentInput{
		Name: "  test agent  ", Role: "custom", Scopes: []string{"queue.read", "queue.read"},
		AccountIDs: []string{" a1 ", "a1"}, ChatIDs: []string{"-1001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name != "test agent" || len(agent.Scopes) != 1 || agent.AccountIDs[0] != "a1" {
		t.Fatalf("agent normalization = %+v", agent)
	}
	token, scopes, _, err := s.IssueClientToken(ctx, agent.ClientID, secret, "queue.read")
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0] != "queue.read" {
		t.Fatalf("issued scopes = %v", scopes)
	}
	if _, _, _, err := s.IssueClientToken(ctx, agent.ClientID, secret, "message.send"); err == nil {
		t.Fatal("issued a scope the agent was not granted")
	}
	if _, err := s.VerifyToken(ctx, token, nil); err != nil {
		t.Fatalf("verify current token: %v", err)
	}
	if _, err := s.UpdateAgent(ctx, agent.ID, AgentInput{Name: "updated", Role: "custom", Scopes: []string{"monitor.read"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyToken(ctx, token, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("verify token after permission change = %v, want invalid token", err)
	}
}

func TestWildcardResourceGrantsIncludeResourcesAddedLater(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	assertFutureResources := func(t *testing.T, agent Agent) {
		t.Helper()
		for _, accountID := range []string{"account-added-later", "another-new-account"} {
			if !agent.AllowsAccount(accountID) {
				t.Errorf("wildcard account grant does not allow %q", accountID)
			}
		}
		for _, chatID := range []string{"chat-added-later", "another-new-chat"} {
			if !agent.AllowsChat(chatID) {
				t.Errorf("wildcard chat grant does not allow %q", chatID)
			}
		}
	}
	input := AgentInput{Name: "all-resources", Role: "custom", Scopes: []string{"queue.read"}, AccountIDs: []string{"*"}, ChatIDs: []string{"*"}}
	created, _, err := s.CreateAgent(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Agent(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertFutureResources(t, saved)

	limited, _, err := s.CreateAgent(ctx, AgentInput{
		Name: "limited-resources", Role: "custom", Scopes: []string{"queue.read"},
		AccountIDs: []string{"account-1"}, ChatIDs: []string{"chat-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	input.Name = "all-after-edit"
	updated, err := s.UpdateAgent(ctx, limited.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	saved, err = s.Agent(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertFutureResources(t, saved)
}

func TestApprovalPayloadIsEncryptedAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent := createTestAgent(t, s)
	payload := map[string]any{"title": "private test topic", "chatId": "-1001"}
	approval, err := s.CreateApproval(ctx, agent.ID, "topics.create", "-1001", "create test topic", payload)
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	if err := s.db.QueryRowContext(ctx, `SELECT payload_ciphertext FROM tgdl_mcp_approvals WHERE id=?`, approval.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, "private test topic") {
		t.Fatal("approval payload was stored in plaintext")
	}
	items, err := s.ListApprovals(ctx, true)
	if err != nil || len(items) != 1 {
		t.Fatalf("list approvals = %d, %v", len(items), err)
	}
	var got map[string]any
	if err := json.Unmarshal(items[0].Payload, &got); err != nil || got["title"] != payload["title"] {
		t.Fatalf("round-tripped payload = %v, %v", got, err)
	}
	if _, err := s.DecideApproval(ctx, approval.ID, "approved", "admin"); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListApprovals(ctx, true)
	if err != nil || len(items) != 0 {
		t.Fatalf("pending approvals after decision = %d, %v", len(items), err)
	}
}

func TestIdempotencyReplaysOnlyCompletedMatchingPayload(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent := createTestAgent(t, s)
	payload := map[string]any{"group": "-1001"}
	if _, replay, err := s.BeginIdempotency(ctx, agent.ID, "operation-001", payload); err != nil || replay {
		t.Fatalf("first begin = replay %v, err %v", replay, err)
	}
	if _, replay, err := s.BeginIdempotency(ctx, agent.ID, "operation-001", payload); err == nil || replay {
		t.Fatalf("pending duplicate = replay %v, err %v", replay, err)
	}
	if err := s.CompleteIdempotency(ctx, agent.ID, "operation-001", map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	result, replay, err := s.BeginIdempotency(ctx, agent.ID, "operation-001", payload)
	if err != nil || !replay || string(result) != `{"ok":true}` {
		t.Fatalf("completed replay = %s, %v, %v", result, replay, err)
	}
	if _, _, err := s.BeginIdempotency(ctx, agent.ID, "operation-001", map[string]any{"group": "-1002"}); err == nil {
		t.Fatal("accepted a reused idempotency key with a different payload")
	}
}
