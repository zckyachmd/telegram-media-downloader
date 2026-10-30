package mcpcontrol

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	TokenTTL       = time.Hour
	ApprovalTTL    = 10 * time.Minute
	AuditRetention = 365 * 24 * time.Hour
)

var knownScopes = map[string]bool{
	"monitor.read": true, "monitor.write": true,
	"queue.read": true, "queue.retry": true, "queue.retry_all": true,
	"forward.read": true, "forward.retry": true,
	"group.read": true, "group.write": true,
	"topic.read": true, "topic.write": true,
	"message.send": true, "message.edit": true, "message.read": true,
	"approval.read": true, "approval.resolve": true,
}

var roleScopes = map[string][]string{
	"read-only": {"monitor.read", "queue.read", "forward.read", "group.read", "topic.read"},
	"operator":  {"monitor.read", "monitor.write", "queue.read", "queue.retry", "forward.read", "forward.retry", "group.read", "group.write", "topic.read", "topic.write", "message.send", "message.edit"},
	"admin":     {"monitor.read", "monitor.write", "queue.read", "queue.retry", "queue.retry_all", "forward.read", "forward.retry", "group.read", "group.write", "topic.read", "topic.write", "message.send", "message.edit", "message.read", "approval.read", "approval.resolve"},
}

type Agent struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Role       string   `json:"role"`
	ClientID   string   `json:"clientId"`
	Scopes     []string `json:"scopes"`
	AccountIDs []string `json:"accountIds"`
	ChatIDs    []string `json:"chatIds"`
	Active     bool     `json:"active"`
	CreatedAt  int64    `json:"createdAt"`
	UpdatedAt  int64    `json:"updatedAt"`
	RevokedAt  int64    `json:"revokedAt,omitempty"`
}

type AgentInput struct {
	Name       string   `json:"name"`
	Role       string   `json:"role"`
	Scopes     []string `json:"scopes"`
	AccountIDs []string `json:"accountIds"`
	ChatIDs    []string `json:"chatIds"`
}

type Approval struct {
	ID          string          `json:"id"`
	RequesterID string          `json:"requesterAgentId"`
	Operation   string          `json:"operation"`
	Target      string          `json:"target"`
	Summary     string          `json:"summary"`
	PayloadHash string          `json:"payloadHash"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Status      string          `json:"status"`
	CreatedAt   int64           `json:"createdAt"`
	ExpiresAt   int64           `json:"expiresAt"`
	DecidedAt   int64           `json:"decidedAt,omitempty"`
	DecidedBy   string          `json:"decidedBy,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

type Store struct {
	db  *sql.DB
	key []byte
}

func New(db *sql.DB, key []byte) (*Store, error) {
	if db == nil || len(key) < 32 {
		return nil, errors.New("MCP store requires a database and a 32-byte encryption key")
	}
	keyCopy := append([]byte(nil), key[:32]...)
	s := &Store{db: db, key: keyCopy}
	now := time.Now().Unix()
	if _, err := db.Exec(`DELETE FROM tgdl_mcp_audit WHERE occurred_at < ?`, now-int64(AuditRetention.Seconds())); err != nil {
		return nil, fmt.Errorf("expire MCP audit records: %w", err)
	}
	if _, err := db.Exec(`UPDATE tgdl_mcp_approvals SET status='expired' WHERE status='pending' AND expires_at<=?`, now); err != nil {
		return nil, fmt.Errorf("expire MCP approval requests: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM tgdl_mcp_tokens WHERE expires_at<=? OR revoked_at IS NOT NULL`, now); err != nil {
		return nil, fmt.Errorf("prune MCP access tokens: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM tgdl_mcp_idempotency WHERE created_at<?`, now-int64((30*24*time.Hour).Seconds())); err != nil {
		return nil, fmt.Errorf("prune MCP idempotency records: %w", err)
	}
	return s, nil
}

func (s *Store) Close() {
	for i := range s.key {
		s.key[i] = 0
	}
}

func PayloadDigest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return digestBytes(encoded)
}

func (s *Store) BeginIdempotency(ctx context.Context, agentID, key string, payload any) (json.RawMessage, bool, error) {
	key = strings.TrimSpace(key)
	if len(key) < 8 || len(key) > 128 {
		return nil, false, errors.New("idempotencyKey must contain 8-128 characters")
	}
	hash := PayloadDigest(payload)
	if hash == "" {
		return nil, false, errors.New("could not hash operation payload")
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO tgdl_mcp_idempotency(agent_id,idempotency_key,payload_hash,result_json,created_at) VALUES(?,?,?,'{"pending":true}',?)`, agentID, key, hash, time.Now().Unix())
	if err != nil {
		return nil, false, err
	}
	if n, _ := result.RowsAffected(); n == 1 {
		return nil, false, nil
	}
	var previousHash, response string
	if err := s.db.QueryRowContext(ctx, `SELECT payload_hash,result_json FROM tgdl_mcp_idempotency WHERE agent_id=? AND idempotency_key=?`, agentID, key).Scan(&previousHash, &response); err != nil {
		return nil, false, err
	}
	if previousHash != hash {
		return nil, false, errors.New("idempotencyKey was already used for a different operation")
	}
	if response == `{"pending":true}` {
		return nil, false, errors.New("operation with this idempotencyKey is still pending")
	}
	return json.RawMessage(response), true, nil
}

func (s *Store) CompleteIdempotency(ctx context.Context, agentID, key string, result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE tgdl_mcp_idempotency SET result_json=? WHERE agent_id=? AND idempotency_key=?`, string(encoded), agentID, strings.TrimSpace(key))
	return err
}

func LoadOrCreateKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "mcp.key")
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("MCP key must be exactly 32 bytes")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read MCP key: %w", err)
	}
	key = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dataDir, ".mcp-key-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary MCP key: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write MCP key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync MCP key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateKey(dataDir)
		}
		return nil, fmt.Errorf("install MCP key: %w", err)
	}
	return key, nil
}

func Presets() map[string][]string {
	out := make(map[string][]string, len(roleScopes)+1)
	for role, scopes := range roleScopes {
		out[role] = append([]string(nil), scopes...)
	}
	out["custom"] = sortedScopes(knownScopes)
	return out
}

func (s *Store) CreateAgent(ctx context.Context, in AgentInput) (Agent, string, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return Agent{}, "", errors.New("agent name must contain 1-100 characters")
	}
	if in.Role != "read-only" && in.Role != "operator" && in.Role != "admin" && in.Role != "custom" {
		return Agent{}, "", errors.New("role must be read-only, operator, admin, or custom")
	}
	scopes, err := normalizeScopes(in.Role, in.Scopes)
	if err != nil {
		return Agent{}, "", err
	}
	in.AccountIDs = normalizeIDs(in.AccountIDs)
	in.ChatIDs = normalizeIDs(in.ChatIDs)
	id, err := randomToken(18)
	if err != nil {
		return Agent{}, "", err
	}
	clientID, err := randomToken(18)
	if err != nil {
		return Agent{}, "", err
	}
	secret, err := randomToken(32)
	if err != nil {
		return Agent{}, "", err
	}
	secretHash := digest(secret)
	encodedScopes, _ := json.Marshal(scopes)
	encodedAccounts, _ := json.Marshal(in.AccountIDs)
	encodedChats, _ := json.Marshal(in.ChatIDs)
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, "", fmt.Errorf("create MCP agent: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO tgdl_mcp_agents(id,name,role,client_id,client_secret_hash,scopes_json,account_ids_json,chat_ids_json,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`, id, in.Name, in.Role, clientID, secretHash, string(encodedScopes), string(encodedAccounts), string(encodedChats), now, now)
	if err != nil {
		return Agent{}, "", fmt.Errorf("create MCP agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, "", fmt.Errorf("commit MCP agent creation: %w", err)
	}
	return Agent{ID: id, Name: in.Name, Role: in.Role, ClientID: clientID, Scopes: scopes, AccountIDs: in.AccountIDs, ChatIDs: in.ChatIDs, Active: true, CreatedAt: now, UpdatedAt: now}, secret, nil
}

func (s *Store) UpdateAgent(ctx context.Context, id string, in AgentInput) (Agent, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return Agent{}, errors.New("agent name must contain 1-100 characters")
	}
	if in.Role != "read-only" && in.Role != "operator" && in.Role != "admin" && in.Role != "custom" {
		return Agent{}, errors.New("role must be read-only, operator, admin, or custom")
	}
	scopes, err := normalizeScopes(in.Role, in.Scopes)
	if err != nil {
		return Agent{}, err
	}
	in.AccountIDs = normalizeIDs(in.AccountIDs)
	in.ChatIDs = normalizeIDs(in.ChatIDs)
	encodedScopes, _ := json.Marshal(scopes)
	encodedAccounts, _ := json.Marshal(in.AccountIDs)
	encodedChats, _ := json.Marshal(in.ChatIDs)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, fmt.Errorf("update MCP agent: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE tgdl_mcp_agents SET name=?,role=?,scopes_json=?,account_ids_json=?,chat_ids_json=?,updated_at=? WHERE id=? AND active=1`, in.Name, in.Role, string(encodedScopes), string(encodedAccounts), string(encodedChats), time.Now().Unix(), id)
	if err != nil {
		return Agent{}, fmt.Errorf("update MCP agent: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Agent{}, sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tgdl_mcp_tokens SET revoked_at=? WHERE agent_id=? AND revoked_at IS NULL`, time.Now().Unix(), id); err != nil {
		return Agent{}, err
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, err
	}
	return s.Agent(ctx, id)
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,role,client_id,scopes_json,account_ids_json,chat_ids_json,active,created_at,updated_at,COALESCE(revoked_at,0) FROM tgdl_mcp_agents ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, agent)
	}
	return out, rows.Err()
}

func (s *Store) Agent(ctx context.Context, id string) (Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,role,client_id,scopes_json,account_ids_json,chat_ids_json,active,created_at,updated_at,COALESCE(revoked_at,0) FROM tgdl_mcp_agents WHERE id=?`, id)
	return scanAgent(row)
}

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var agent Agent
	var scopes, accounts, chats string
	var active int
	err := row.Scan(&agent.ID, &agent.Name, &agent.Role, &agent.ClientID, &scopes, &accounts, &chats, &active, &agent.CreatedAt, &agent.UpdatedAt, &agent.RevokedAt)
	if err != nil {
		return Agent{}, err
	}
	agent.Active = active == 1
	_ = json.Unmarshal([]byte(scopes), &agent.Scopes)
	_ = json.Unmarshal([]byte(accounts), &agent.AccountIDs)
	_ = json.Unmarshal([]byte(chats), &agent.ChatIDs)
	return agent, nil
}

func (s *Store) RevokeAgent(ctx context.Context, id string) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE tgdl_mcp_agents SET active=0,revoked_at=?,updated_at=? WHERE id=? AND active=1`, now, now, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_mcp_tokens SET revoked_at=? WHERE agent_id=? AND revoked_at IS NULL`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RotateSecret(ctx context.Context, id string) (string, error) {
	secret, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE tgdl_mcp_agents SET client_secret_hash=?,updated_at=? WHERE id=? AND active=1`, digest(secret), now, id)
	if err != nil {
		return "", err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return "", sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_mcp_tokens SET revoked_at=? WHERE agent_id=? AND revoked_at IS NULL`, now, id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return secret, nil
}

func (s *Store) IssueClientToken(ctx context.Context, clientID, secret, scopeText string) (string, []string, time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	defer tx.Rollback()
	var id, secretHash, scopeJSON string
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT id,client_secret_hash,scopes_json,active FROM tgdl_mcp_agents WHERE client_id=?`, clientID).Scan(&id, &secretHash, &scopeJSON, &active); err != nil || active != 1 {
		return "", nil, time.Time{}, errors.New("invalid client credentials")
	}
	if !constantDigestEqual(secretHash, digest(secret)) {
		return "", nil, time.Time{}, errors.New("invalid client credentials")
	}
	var allowed []string
	if err := json.Unmarshal([]byte(scopeJSON), &allowed); err != nil {
		return "", nil, time.Time{}, errors.New("invalid registered scopes")
	}
	wanted := allowed
	if strings.TrimSpace(scopeText) != "" {
		wanted = strings.Fields(scopeText)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, scope := range allowed {
		allowedSet[scope] = true
	}
	seen := map[string]bool{}
	for _, scope := range wanted {
		if !allowedSet[scope] || seen[scope] {
			return "", nil, time.Time{}, errors.New("requested scope is not granted to this agent")
		}
		seen[scope] = true
	}
	wanted = uniqueSorted(wanted)
	token, err := randomToken(32)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	expires := time.Now().Add(TokenTTL).UTC()
	encoded, _ := json.Marshal(wanted)
	_, err = tx.ExecContext(ctx, `INSERT INTO tgdl_mcp_tokens(token_hash,agent_id,scopes_json,created_at,expires_at) VALUES(?,?,?,?,?)`, digest(token), id, string(encoded), time.Now().Unix(), expires.Unix())
	if err != nil {
		return "", nil, time.Time{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tgdl_mcp_audit(occurred_at,actor_type,actor_id,action,target,change_summary,payload_hash,outcome,approval_id) VALUES(?,?,?,?,?,?,?,?,?)`, time.Now().Unix(), "agent", id, "oauth.token.issue", id, fmt.Sprintf("issued an access token with %d scopes", len(wanted)), "", "succeeded", "")
	if err != nil {
		return "", nil, time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, time.Time{}, err
	}
	return token, wanted, expires, nil
}

func (s *Store) VerifyToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	var id, tokenScopes, agentScopes string
	var expires int64
	var active int
	err := s.db.QueryRowContext(ctx, `SELECT t.agent_id,t.scopes_json,t.expires_at,a.scopes_json,a.active FROM tgdl_mcp_tokens t JOIN tgdl_mcp_agents a ON a.id=t.agent_id WHERE t.token_hash=? AND t.revoked_at IS NULL`, digest(token)).Scan(&id, &tokenScopes, &expires, &agentScopes, &active)
	if err != nil || active != 1 || expires <= time.Now().Unix() {
		return nil, fmt.Errorf("%w: invalid bearer token", auth.ErrInvalidToken)
	}
	var tokenScopeList, agentScopeList []string
	if json.Unmarshal([]byte(tokenScopes), &tokenScopeList) != nil || json.Unmarshal([]byte(agentScopes), &agentScopeList) != nil {
		return nil, fmt.Errorf("%w: invalid bearer token scopes", auth.ErrInvalidToken)
	}
	current := map[string]bool{}
	for _, scope := range agentScopeList {
		current[scope] = true
	}
	var scopes []string
	for _, scope := range tokenScopeList {
		if current[scope] {
			scopes = append(scopes, scope)
		}
	}
	return &auth.TokenInfo{UserID: id, Scopes: scopes, Expiration: time.Unix(expires, 0), Extra: map[string]any{"agent_id": id}}, nil
}

func (s *Store) Audit(ctx context.Context, actorType, actorID, action, target, summary, payloadHash, outcome, approvalID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tgdl_mcp_audit(occurred_at,actor_type,actor_id,action,target,change_summary,payload_hash,outcome,approval_id) VALUES(?,?,?,?,?,?,?,?,?)`, time.Now().Unix(), actorType, actorID, action, target, summary, payloadHash, outcome, approvalID)
	return err
}

func (s *Store) AuditEntries(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,occurred_at,actor_type,actor_id,action,target,change_summary,payload_hash,outcome,approval_id FROM tgdl_mcp_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, occurred int64
		var actorType, actorID, action, target, summary, payloadHash, outcome, approvalID string
		if err := rows.Scan(&id, &occurred, &actorType, &actorID, &action, &target, &summary, &payloadHash, &outcome, &approvalID); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "occurredAt": occurred, "actorType": actorType, "actorId": actorID, "action": action, "target": target, "summary": summary, "payloadHash": payloadHash, "outcome": outcome, "approvalId": approvalID})
	}
	return out, rows.Err()
}

func (s *Store) CreateApproval(ctx context.Context, requester, operation, target, summary string, payload any) (Approval, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return Approval{}, err
	}
	payloadHash := digestBytes(plain)
	ciphertext, err := s.encrypt(plain)
	if err != nil {
		return Approval{}, err
	}
	id, err := randomToken(18)
	if err != nil {
		return Approval{}, err
	}
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO tgdl_mcp_approvals(id,requester_agent_id,operation,target,summary,payload_hash,payload_ciphertext,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,'pending',?,?)`, id, requester, operation, target, summary, payloadHash, ciphertext, now.Unix(), now.Add(ApprovalTTL).Unix())
	if err != nil {
		return Approval{}, err
	}
	return Approval{ID: id, RequesterID: requester, Operation: operation, Target: target, Summary: summary, PayloadHash: payloadHash, Payload: plain, Status: "pending", CreatedAt: now.Unix(), ExpiresAt: now.Add(ApprovalTTL).Unix()}, nil
}

func (s *Store) ListApprovals(ctx context.Context, pendingOnly bool) ([]Approval, error) {
	query := `SELECT id,requester_agent_id,operation,target,summary,payload_hash,payload_ciphertext,status,created_at,expires_at,COALESCE(decided_at,0),decided_by,result_json FROM tgdl_mcp_approvals`
	if pendingOnly {
		query += ` WHERE status='pending' AND expires_at>?`
	}
	query += ` ORDER BY created_at DESC LIMIT 200`
	var rows *sql.Rows
	var err error
	if pendingOnly {
		rows, err = s.db.QueryContext(ctx, query, time.Now().Unix())
	} else {
		rows, err = s.db.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		item, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		plain, err := s.decrypt(item.Payload)
		if err != nil {
			return nil, err
		}
		if digestBytes(plain) != item.PayloadHash {
			return nil, errors.New("approval payload integrity check failed")
		}
		item.Payload = plain
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DecideApproval(ctx context.Context, id, decision, decidedBy string) (Approval, error) {
	if decision != "approved" && decision != "rejected" {
		return Approval{}, errors.New("decision must be approved or rejected")
	}
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE tgdl_mcp_approvals SET status=?,decided_at=?,decided_by=? WHERE id=? AND status='pending' AND expires_at>?`, decision, now, decidedBy, id, now)
	if err != nil {
		return Approval{}, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Approval{}, sql.ErrNoRows
	}
	return s.Approval(ctx, id)
}

func (s *Store) Approval(ctx context.Context, id string) (Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,requester_agent_id,operation,target,summary,payload_hash,payload_ciphertext,status,created_at,expires_at,COALESCE(decided_at,0),decided_by,result_json FROM tgdl_mcp_approvals WHERE id=?`, id)
	item, err := scanApproval(row)
	if err != nil {
		return Approval{}, err
	}
	plain, err := s.decrypt(item.Payload)
	if err != nil {
		return Approval{}, err
	}
	if digestBytes(plain) != item.PayloadHash {
		return Approval{}, errors.New("approval payload integrity check failed")
	}
	item.Payload = plain
	return item, nil
}

func scanApproval(row interface{ Scan(...any) error }) (Approval, error) {
	var item Approval
	var encrypted []byte
	var result string
	err := row.Scan(&item.ID, &item.RequesterID, &item.Operation, &item.Target, &item.Summary, &item.PayloadHash, &encrypted, &item.Status, &item.CreatedAt, &item.ExpiresAt, &item.DecidedAt, &item.DecidedBy, &result)
	if err != nil {
		return Approval{}, err
	}
	item.Payload = encrypted
	if result != "" {
		item.Result = json.RawMessage(result)
	}
	return item, nil
}

func (s *Store) CompleteApproval(ctx context.Context, id, status string, result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE tgdl_mcp_approvals SET status=?,result_json=? WHERE id=? AND status='approved'`, status, string(encoded), id)
	return err
}

func (s *Store) encrypt(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, aead.Seal(nil, nonce, plain, []byte("tgdl-mcp-approval-v1"))...), nil
}
func (s *Store) decrypt(sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("invalid approval ciphertext")
	}
	nonce, body := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, body, []byte("tgdl-mcp-approval-v1"))
}

func (a Agent) AllowsScope(scope string) bool {
	for _, v := range a.Scopes {
		if v == scope {
			return true
		}
	}
	return false
}
func (a Agent) AllowsAccount(id string) bool { return allowsID(a.AccountIDs, id) }
func (a Agent) AllowsChat(id string) bool    { return allowsID(a.ChatIDs, id) }

func normalizeScopes(role string, scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		scopes = roleScopes[role]
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if !knownScopes[scope] {
			return nil, fmt.Errorf("unknown MCP scope %q", scope)
		}
		seen[scope] = true
	}
	return uniqueSorted(scopes), nil
}
func normalizeIDs(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
func uniqueSorted(values []string) []string { return normalizeIDs(values) }
func sortedScopes(set map[string]bool) []string {
	var out []string
	for scope := range set {
		out = append(out, scope)
	}
	return uniqueSorted(out)
}
func allowsID(allowed []string, id string) bool {
	for _, v := range allowed {
		if v == "*" || v == id {
			return true
		}
	}
	return false
}
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func digest(v string) string      { return digestBytes([]byte(v)) }
func digestBytes(v []byte) string { sum := sha256.Sum256(v); return hex.EncodeToString(sum[:]) }
func constantDigestEqual(a, b string) bool {
	left, err := hex.DecodeString(a)
	if err != nil {
		return false
	}
	right, err := hex.DecodeString(b)
	if err != nil || len(left) != len(right) {
		return false
	}
	var x byte
	for i := range left {
		x |= left[i] ^ right[i]
	}
	return x == 0
}
