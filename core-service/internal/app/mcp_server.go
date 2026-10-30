package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/botnick/telegram-media-downloader/core-service/internal/mcpcontrol"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpEmpty struct{}
type mcpGroupInput struct {
	GroupID        string         `json:"groupId" jsonschema:"Telegram chat ID being configured"`
	Patch          map[string]any `json:"patch" jsonschema:"Partial monitoring group configuration"`
	IdempotencyKey string         `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpGroupRemoveInput struct {
	GroupID        string `json:"groupId" jsonschema:"Monitored chat ID to remove from application configuration"`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpQueueRetryInput struct {
	Key            string `json:"key" jsonschema:"Queue key in groupId_messageId format"`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpQueueRetryAllInput struct {
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpForwardRetryInput struct {
	ID             int64  `json:"id" jsonschema:"Forward queue item ID"`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpApprovalDecisionInput struct {
	ApprovalID string `json:"approvalId" jsonschema:"Pending approval ID"`
	Decision   string `json:"decision" jsonschema:"approved or rejected"`
}
type mcpApprovalListInput struct {
	PendingOnly bool `json:"pendingOnly" jsonschema:"Return pending approvals only"`
}
type mcpTopicListInput struct {
	AccountID string `json:"accountId" jsonschema:"Connected Telegram account ID"`
	GroupID   string `json:"groupId" jsonschema:"Forum chat ID"`
}
type mcpTopicCreateInput struct {
	AccountID      string `json:"accountId" jsonschema:"Connected Telegram account ID"`
	GroupID        string `json:"groupId" jsonschema:"Forum chat ID"`
	Title          string `json:"title" jsonschema:"Topic title, 1-128 characters"`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpTopicBulkCreateInput struct {
	AccountID      string   `json:"accountId" jsonschema:"Connected Telegram account ID"`
	GroupID        string   `json:"groupId" jsonschema:"Forum chat ID"`
	Titles         []string `json:"titles" jsonschema:"Topic titles, maximum 50"`
	IdempotencyKey string   `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpTopicUpdate struct {
	TopicID     int     `json:"topicId" jsonschema:"Telegram topic ID"`
	Title       *string `json:"title,omitempty" jsonschema:"Optional replacement title"`
	Closed      *bool   `json:"closed,omitempty" jsonschema:"Optional closed state"`
	IconEmojiID *int64  `json:"iconEmojiId,omitempty" jsonschema:"Optional Telegram custom emoji ID; use 0 for default icon"`
	Hidden      *bool   `json:"hidden,omitempty" jsonschema:"Optional hidden state; Telegram only allows this for General topic"`
}
type mcpTopicUpdateInput struct {
	AccountID      string  `json:"accountId" jsonschema:"Connected Telegram account ID"`
	GroupID        string  `json:"groupId" jsonschema:"Forum chat ID"`
	TopicID        int     `json:"topicId" jsonschema:"Telegram topic ID"`
	Title          *string `json:"title,omitempty" jsonschema:"Optional replacement title"`
	Closed         *bool   `json:"closed,omitempty" jsonschema:"Optional closed state"`
	IconEmojiID    *int64  `json:"iconEmojiId,omitempty" jsonschema:"Optional Telegram custom emoji ID; use 0 for default icon"`
	Hidden         *bool   `json:"hidden,omitempty" jsonschema:"Optional hidden state; Telegram only allows this for General topic"`
	IdempotencyKey string  `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpTopicBulkUpdateInput struct {
	AccountID      string           `json:"accountId" jsonschema:"Connected Telegram account ID"`
	GroupID        string           `json:"groupId" jsonschema:"Forum chat ID"`
	Updates        []mcpTopicUpdate `json:"updates" jsonschema:"Topic updates, maximum 50"`
	IdempotencyKey string           `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpMessageInput struct {
	AccountID      string `json:"accountId" jsonschema:"Connected Telegram account ID"`
	GroupID        string `json:"groupId" jsonschema:"Telegram chat ID"`
	TopicID        int    `json:"topicId,omitempty" jsonschema:"Optional topic ID for forum replies"`
	Message        string `json:"message" jsonschema:"Plain text message, maximum 4096 characters"`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}
type mcpMessageEditInput struct {
	AccountID      string `json:"accountId" jsonschema:"Connected Telegram account ID"`
	GroupID        string `json:"groupId" jsonschema:"Telegram chat ID"`
	MessageID      int    `json:"messageId" jsonschema:"Telegram message ID"`
	Message        string `json:"message" jsonschema:"Replacement plain text, maximum 4096 characters"`
	IdempotencyKey string `json:"idempotencyKey" jsonschema:"Unique request key, 8-128 characters"`
}

func (a *App) MCPHandler() http.Handler {
	issuer := strings.TrimRight(a.mcpIssuer, "/")
	if issuer == "" {
		issuer = "http://127.0.0.1:3001"
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "telegram-media-downloader", Title: "Telegram Media Downloader", Version: "3"}, nil)
	a.registerMCPTools(server)
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		SessionTimeout: 10 * time.Minute, MaxRequestBodyBytes: 1 << 20, PropagateRequestCancellation: true,
	})
	protected := sdkauth.RequireBearerToken(a.mcpControl.VerifyToken, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: issuer + "/.well-known/oauth-protected-resource/mcp",
	})(stream)
	oauth := a.mcpControl.OAuthHandler(issuer)
	mux := http.NewServeMux()
	mux.Handle("/mcp", protected)
	mux.Handle("/mcp/", protected)
	mux.Handle("/oauth/", oauth)
	mux.Handle("/.well-known/", oauth)
	return http.NewCrossOriginProtection().Handler(mux)
}

func (a *App) registerMCPTools(server *mcp.Server) {
	addMCPMapTool(server, a.mcpControl, "agent.self", "Show the authenticated agent's current role, permissions, and resource limits.", true, func(ctx context.Context, _ mcpEmpty) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "")
		if err != nil {
			return nil, err
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "agent.self", "", "read own agent permissions", "", "succeeded", ""); err != nil {
			return nil, errors.New("audit unavailable")
		}
		return map[string]any{"agent": agent}, nil
	})
	addMCPMapTool(server, a.mcpControl, "monitor.status", "Read monitor process status without exposing account session data.", true, func(ctx context.Context, _ mcpEmpty) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "monitor.read")
		if err != nil {
			return nil, err
		}
		status, err := a.monitor.Status(ctx)
		if err != nil {
			return nil, err
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "monitor.status", "", "read monitor status", "", "succeeded", ""); err != nil {
			return nil, errors.New("audit unavailable")
		}
		return map[string]any{"status": status}, nil
	})
	addMCPMapTool(server, a.mcpControl, "groups.list", "List configured monitoring chats and their configuration within the agent's allowed chat and account scope.", true, func(ctx context.Context, _ mcpEmpty) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "group.read")
		if err != nil {
			return nil, err
		}
		cfg, err := a.config.Load(ctx)
		if err != nil {
			return nil, err
		}
		groups, _ := cfg["groups"].([]any)
		out := make([]any, 0, len(groups))
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id := toString(group["id"])
			if !agent.AllowsChat(id) || !a.mcpChatAccountsAllowed(ctx, agent, id) {
				continue
			}
			out = append(out, filterMCPGroup(cloneConfigValue(group).(map[string]any), agent))
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "groups.list", "", "read scoped monitoring group configuration", "", "succeeded", ""); err != nil {
			return nil, errors.New("audit unavailable")
		}
		return map[string]any{"groups": out}, nil
	})
	addMCPMapTool(server, a.mcpControl, "groups.upsert", "Create or update one application's monitored-chat configuration. Does not create or delete Telegram chats.", false, func(ctx context.Context, in mcpGroupInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "group.write")
		if err != nil {
			return nil, err
		}
		id := strings.TrimSpace(in.GroupID)
		if id == "" || !agent.AllowsChat(id) || !a.mcpChatAccountsAllowed(ctx, agent, id) {
			return nil, errors.New("agent is not allowed to change this chat")
		}
		if len(in.Patch) == 0 {
			return nil, errors.New("group patch must contain at least one allowed setting")
		}
		if err := validateMCPGroupPatch(agent, in.Patch); err != nil {
			return nil, err
		}
		payload := map[string]any{"groupId": id, "patch": in.Patch}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "groups.upsert", id, "requested monitoring configuration change", mcpcontrol.PayloadDigest(payload), "requested", ""); err != nil {
			return nil, errors.New("audit unavailable; configuration was not changed")
		}
		if cached, hit, err := a.mcpControl.BeginIdempotency(ctx, agent.ID, in.IdempotencyKey, payload); err != nil {
			return nil, err
		} else if hit {
			var out map[string]any
			_ = json.Unmarshal(cached, &out)
			return out, nil
		}
		out, err := callMCPAppHandler(ctx, "PUT", "/api/groups/"+id, map[string]string{"id": id}, in.Patch, a.handleAPIGroupSave)
		if err != nil {
			_ = a.mcpControl.Audit(ctx, "agent", agent.ID, "groups.upsert", id, "monitoring configuration change failed", mcpcontrol.PayloadDigest(payload), "failed", "")
			return nil, err
		}
		if err := a.mcpControl.CompleteIdempotency(ctx, agent.ID, in.IdempotencyKey, out); err != nil {
			return nil, errors.New("configuration changed but idempotency recording failed")
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "groups.upsert", id, "updated monitoring configuration", mcpcontrol.PayloadDigest(payload), "succeeded", ""); err != nil {
			return nil, errors.New("operation succeeded but audit write failed")
		}
		return out, nil
	})
	addMCPMapTool(server, a.mcpControl, "groups.remove", "Request removal of a chat from the application's monitoring configuration. Telegram data is not deleted; operator approval is required.", false, func(ctx context.Context, in mcpGroupRemoveInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "group.write")
		if err != nil {
			return nil, err
		}
		id := strings.TrimSpace(in.GroupID)
		if id == "" || !agent.AllowsChat(id) || !a.mcpChatAccountsAllowed(ctx, agent, id) {
			return nil, errors.New("agent is not allowed to change this chat")
		}
		payload := map[string]any{"groupId": id}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "groups.remove", id, "Remove this chat from application monitoring configuration", payload)
	})
	addMCPMapTool(server, a.mcpControl, "topics.list", "List forum topics using one selected connected Telegram account.", true, func(ctx context.Context, in mcpTopicListInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "topic.read")
		if err != nil {
			return nil, err
		}
		if !a.mcpChatAccountAllowed(ctx, agent, in.GroupID, in.AccountID) {
			return nil, errors.New("agent is not allowed to read topics for this chat/account")
		}
		session, err := a.monitor.OpenDialogs(ctx)
		if err != nil {
			return nil, err
		}
		defer session.Close()
		opCtx := session.Context()
		account, err := session.ChatAccount(opCtx, in.AccountID)
		if err != nil {
			return nil, err
		}
		topics, err := account.ForumTopics(opCtx, in.GroupID)
		if err != nil {
			return nil, err
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "topics.list", in.GroupID, "read forum topics", "", "succeeded", ""); err != nil {
			return nil, errors.New("audit unavailable")
		}
		return map[string]any{"topics": topics}, nil
	})
	addMCPMapTool(server, a.mcpControl, "topics.create", "Request creation of one Telegram forum topic. Requires a separate operator/admin approval.", false, func(ctx context.Context, in mcpTopicCreateInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "topic.write")
		if err != nil {
			return nil, err
		}
		if !validMCPTopicTitle(in.Title) || !a.mcpChatAccountAllowed(ctx, agent, in.GroupID, in.AccountID) {
			return nil, errors.New("invalid topic title or unauthorized chat/account")
		}
		payload := map[string]any{"accountId": in.AccountID, "groupId": in.GroupID, "title": strings.TrimSpace(in.Title)}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "topic.create", in.GroupID, "Create one forum topic", payload)
	})
	addMCPMapTool(server, a.mcpControl, "topics.bulk_create", "Preview and request creation of up to 50 Telegram forum topics as one approved operation.", false, func(ctx context.Context, in mcpTopicBulkCreateInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "topic.write")
		if err != nil {
			return nil, err
		}
		if len(in.Titles) == 0 || len(in.Titles) > 50 || !a.mcpChatAccountAllowed(ctx, agent, in.GroupID, in.AccountID) {
			return nil, errors.New("bulk topic create requires 1-50 titles and an authorized chat/account")
		}
		titles := make([]string, len(in.Titles))
		for i, title := range in.Titles {
			if !validMCPTopicTitle(title) {
				return nil, fmt.Errorf("topic title at index %d is invalid", i)
			}
			titles[i] = strings.TrimSpace(title)
		}
		payload := map[string]any{"accountId": in.AccountID, "groupId": in.GroupID, "titles": titles}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "topic.bulk_create", in.GroupID, fmt.Sprintf("Create %d forum topics", len(titles)), payload)
	})
	addMCPMapTool(server, a.mcpControl, "topics.update", "Request an approved title, closed-state, icon, or visibility update for one Telegram forum topic.", false, func(ctx context.Context, in mcpTopicUpdateInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "topic.write")
		if err != nil {
			return nil, err
		}
		if in.TopicID <= 0 || !hasMCPTopicChange(in.Title, in.Closed, in.IconEmojiID, in.Hidden) || (in.Title != nil && !validMCPTopicTitle(*in.Title)) || !a.mcpChatAccountAllowed(ctx, agent, in.GroupID, in.AccountID) {
			return nil, errors.New("invalid topic update or unauthorized chat/account")
		}
		payload := map[string]any{"accountId": in.AccountID, "groupId": in.GroupID, "updates": []mcpTopicUpdate{{TopicID: in.TopicID, Title: in.Title, Closed: in.Closed, IconEmojiID: in.IconEmojiID, Hidden: in.Hidden}}}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "topic.update", in.GroupID, "Update one forum topic", payload)
	})
	addMCPMapTool(server, a.mcpControl, "topics.bulk_update", "Preview and request title, closed-state, icon, or visibility updates for up to 50 Telegram forum topics.", false, func(ctx context.Context, in mcpTopicBulkUpdateInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "topic.write")
		if err != nil {
			return nil, err
		}
		if len(in.Updates) == 0 || len(in.Updates) > 50 || !a.mcpChatAccountAllowed(ctx, agent, in.GroupID, in.AccountID) {
			return nil, errors.New("bulk topic update requires 1-50 changes and an authorized chat/account")
		}
		seen := map[int]bool{}
		for i, update := range in.Updates {
			if update.TopicID <= 0 || seen[update.TopicID] || !hasMCPTopicChange(update.Title, update.Closed, update.IconEmojiID, update.Hidden) || (update.Title != nil && !validMCPTopicTitle(*update.Title)) {
				return nil, fmt.Errorf("topic update at index %d is invalid or duplicated", i)
			}
			seen[update.TopicID] = true
			if update.Title != nil {
				title := strings.TrimSpace(*update.Title)
				in.Updates[i].Title = &title
			}
		}
		payload := map[string]any{"accountId": in.AccountID, "groupId": in.GroupID, "updates": in.Updates}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "topic.bulk_update", in.GroupID, fmt.Sprintf("Update %d forum topics", len(in.Updates)), payload)
	})
	addMCPMapTool(server, a.mcpControl, "messages.send", "Request an approved plain-text Telegram message send.", false, func(ctx context.Context, in mcpMessageInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "message.send")
		if err != nil {
			return nil, err
		}
		if !validMCPMessage(in.Message) || !a.mcpChatAccountAllowed(ctx, agent, in.GroupID, in.AccountID) || in.TopicID < 0 {
			return nil, errors.New("invalid message or unauthorized chat/account")
		}
		payload := map[string]any{"accountId": in.AccountID, "groupId": in.GroupID, "topicId": in.TopicID, "message": in.Message}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "message.send", in.GroupID, "Send a plain-text message", payload)
	})
	addMCPMapTool(server, a.mcpControl, "messages.edit", "Request an approved edit of a Telegram text message.", false, func(ctx context.Context, in mcpMessageEditInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "message.edit")
		if err != nil {
			return nil, err
		}
		if in.MessageID <= 0 || !validMCPMessage(in.Message) || !a.mcpChatAccountAllowed(ctx, agent, in.GroupID, in.AccountID) {
			return nil, errors.New("invalid message edit or unauthorized chat/account")
		}
		payload := map[string]any{"accountId": in.AccountID, "groupId": in.GroupID, "messageId": in.MessageID, "message": in.Message}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "message.edit", in.GroupID, "Edit a Telegram text message", payload)
	})
	addMCPMapTool(server, a.mcpControl, "queue.snapshot", "Read bounded download and forward queue metadata, filtered to the agent's authorized accounts and chats. Local file paths are omitted.", true, func(ctx context.Context, _ mcpEmpty) (map[string]any, error) {
		agent, tokenScopes, err := a.requireMCPAgent(ctx, "queue.read")
		if err != nil {
			return nil, err
		}
		snapshot, err := a.monitor.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		out := a.filterQueueSnapshot(ctx, snapshot, agent)
		forward := map[string]any{"total": 0, "pending": 0, "active": 0, "forwarded": 0, "failed": 0, "interrupted": 0, "items": []map[string]any{}}
		if agent.AllowsScope("forward.read") && tokenHasScope(tokenScopes, "forward.read") {
			forward, err = a.forwardQueueSnapshot(ctx)
			if err != nil {
				return nil, err
			}
		}
		out["forwardQueue"] = a.filterForwardSnapshot(ctx, forward, agent)
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "queue.snapshot", "", "read scoped queue status", "", "succeeded", ""); err != nil {
			return nil, errors.New("audit unavailable")
		}
		return out, nil
	})
	addMCPMapTool(server, a.mcpControl, "queue.retry", "Retry one failed or interrupted download. A duplicate idempotency key returns the previous result.", false, func(ctx context.Context, in mcpQueueRetryInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "queue.retry")
		if err != nil {
			return nil, err
		}
		groupID, accountID, err := a.mcpQueueTarget(ctx, in.Key)
		if err != nil {
			return nil, err
		}
		if !a.mcpChatAccountAllowed(ctx, agent, groupID, accountID) {
			return nil, errors.New("agent is not allowed to retry this queue item")
		}
		payload := map[string]any{"key": in.Key}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "queue.retry", in.Key, "requested one queue retry", mcpcontrol.PayloadDigest(payload), "requested", ""); err != nil {
			return nil, errors.New("audit unavailable; queue item was not retried")
		}
		if cached, hit, err := a.mcpControl.BeginIdempotency(ctx, agent.ID, in.IdempotencyKey, payload); err != nil {
			return nil, err
		} else if hit {
			var out map[string]any
			_ = json.Unmarshal(cached, &out)
			return out, nil
		}
		out, err := callMCPAppHandler(ctx, "POST", "/api/queue/"+in.Key+"/retry", map[string]string{"key": in.Key}, nil, a.handleQueueRetry)
		if err != nil {
			_ = a.mcpControl.Audit(ctx, "agent", agent.ID, "queue.retry", in.Key, "queue retry failed", mcpcontrol.PayloadDigest(payload), "failed", "")
			return nil, err
		}
		if err := a.mcpControl.CompleteIdempotency(ctx, agent.ID, in.IdempotencyKey, out); err != nil {
			return nil, errors.New("queue item was retried but idempotency recording failed")
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "queue.retry", in.Key, "retried one queue item", mcpcontrol.PayloadDigest(payload), "succeeded", ""); err != nil {
			return nil, errors.New("operation succeeded but audit write failed")
		}
		return out, nil
	})
	addMCPMapTool(server, a.mcpControl, "queue.retry_all", "Request approval to retry failed queue items in the agent's authorized scope. Returns a preview and approval ID; successful items are excluded.", false, func(ctx context.Context, in mcpQueueRetryAllInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "queue.retry_all")
		if err != nil {
			return nil, err
		}
		keys, err := a.mcpFailedQueueKeys(ctx, agent, 100)
		if err != nil {
			return nil, err
		}
		if len(keys) == 0 {
			return map[string]any{"status": "no_failed_items", "count": 0}, nil
		}
		payload := map[string]any{"keys": keys}
		return a.mcpRequestApproval(ctx, agent, in.IdempotencyKey, "queue.retry_all", "authorized failed download items", fmt.Sprintf("Retry %d failed queue items", len(keys)), payload)
	})
	addMCPMapTool(server, a.mcpControl, "forward.retry", "Retry delivery for one failed forward-queue item. Telegram destinations already delivered successfully remain skipped by the application pipeline.", false, func(ctx context.Context, in mcpForwardRetryInput) (map[string]any, error) {
		agent, _, err := a.requireMCPAgent(ctx, "forward.retry")
		if err != nil {
			return nil, err
		}
		var groupID string
		if err := a.db.Reader.QueryRowContext(ctx, `SELECT group_id FROM tgdl_forward_queue WHERE id=?`, in.ID).Scan(&groupID); err != nil {
			return nil, err
		}
		if in.ID <= 0 || !agent.AllowsChat(groupID) || !a.mcpChatAccountsAllowed(ctx, agent, groupID) {
			return nil, errors.New("agent is not allowed to retry this forward item")
		}
		forwardSnapshot, err := a.forwardQueueSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		items, _ := forwardSnapshot["items"].([]map[string]any)
		found := false
		for _, item := range items {
			if int64(number(item["id"], 0)) != in.ID {
				continue
			}
			found = true
			destinations, _ := item["destinations"].([]string)
			for _, destination := range destinations {
				if !agent.AllowsChat(destination) || !a.mcpChatAccountsAllowed(ctx, agent, destination) {
					return nil, errors.New("agent is not allowed for every destination of this forward item")
				}
			}
			break
		}
		if !found {
			return nil, errors.New("forward item is outside the agent's visible scope")
		}
		payload := map[string]any{"id": in.ID}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "forward.retry", fmt.Sprint(in.ID), "requested one forward retry", mcpcontrol.PayloadDigest(payload), "requested", ""); err != nil {
			return nil, errors.New("audit unavailable; forward item was not retried")
		}
		if cached, hit, err := a.mcpControl.BeginIdempotency(ctx, agent.ID, in.IdempotencyKey, payload); err != nil {
			return nil, err
		} else if hit {
			var out map[string]any
			_ = json.Unmarshal(cached, &out)
			return out, nil
		}
		out, err := callMCPAppHandler(ctx, "POST", fmt.Sprintf("/api/forward-queue/%d/retry", in.ID), map[string]string{"id": fmt.Sprint(in.ID)}, nil, a.handleForwardQueueRetry)
		if err != nil {
			_ = a.mcpControl.Audit(ctx, "agent", agent.ID, "forward.retry", fmt.Sprint(in.ID), "forward retry failed", mcpcontrol.PayloadDigest(payload), "failed", "")
			return nil, err
		}
		if err := a.mcpControl.CompleteIdempotency(ctx, agent.ID, in.IdempotencyKey, out); err != nil {
			return nil, errors.New("forward item was retried but idempotency recording failed")
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "forward.retry", fmt.Sprint(in.ID), "retried forward item", mcpcontrol.PayloadDigest(payload), "succeeded", ""); err != nil {
			return nil, errors.New("operation succeeded but audit write failed")
		}
		return out, nil
	})
	addMCPMapTool(server, a.mcpControl, "approvals.list", "List pending operation approvals. Approval payloads are only revealed to authorized approvers.", true, func(ctx context.Context, in mcpApprovalListInput) (map[string]any, error) {
		agent, tokenScopes, err := a.requireMCPAgent(ctx, "approval.read")
		if err != nil {
			return nil, err
		}
		items, err := a.mcpControl.ListApprovals(ctx, in.PendingOnly)
		if err != nil {
			return nil, err
		}
		visible := make([]mcpcontrol.Approval, 0, len(items))
		for _, item := range items {
			if a.mcpApprovalAllowed(ctx, agent, item, tokenScopes) {
				visible = append(visible, item)
			}
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "approvals.list", "", "read authorized operation approvals", "", "succeeded", ""); err != nil {
			return nil, errors.New("audit unavailable")
		}
		return map[string]any{"approvals": visible, "viewerAgentId": agent.ID}, nil
	})
	addMCPMapTool(server, a.mcpControl, "approvals.decide", "Approve or reject another agent's pending operation. The requesting agent cannot approve its own request.", false, func(ctx context.Context, in mcpApprovalDecisionInput) (map[string]any, error) {
		agent, tokenScopes, err := a.requireMCPAgent(ctx, "approval.resolve")
		if err != nil {
			return nil, err
		}
		pending, err := a.mcpControl.Approval(ctx, in.ApprovalID)
		if err != nil {
			return nil, err
		}
		if pending.RequesterID == agent.ID {
			return nil, errors.New("an agent cannot approve its own operation")
		}
		if !a.mcpApprovalAllowed(ctx, agent, pending, tokenScopes) {
			return nil, errors.New("approver is not authorized for this operation target")
		}
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, "approval."+in.Decision, pending.Target, "requested approval decision", pending.PayloadHash, "requested", pending.ID); err != nil {
			return nil, errors.New("audit unavailable; approval was not decided")
		}
		decided, err := a.mcpControl.DecideApproval(ctx, in.ApprovalID, in.Decision, agent.ID)
		if err != nil {
			return nil, err
		}
		if in.Decision == "rejected" {
			_ = a.mcpControl.Audit(ctx, "agent", agent.ID, "approval.reject", pending.Target, "rejected another agent's operation", pending.PayloadHash, "rejected", pending.ID)
			return map[string]any{"approval": decided}, nil
		}
		result, executeErr := a.executeMCPApproval(ctx, pending)
		if executeErr != nil {
			_ = a.mcpControl.CompleteApproval(ctx, pending.ID, "failed", map[string]string{"error": executeErr.Error()})
			_ = a.mcpControl.Audit(ctx, "agent", agent.ID, pending.Operation, pending.Target, pending.Summary, pending.PayloadHash, "failed", pending.ID)
			return nil, executeErr
		}
		_ = a.mcpControl.CompleteApproval(ctx, pending.ID, mcpApprovalResultStatus(result), result)
		if err := a.mcpControl.Audit(ctx, "agent", agent.ID, pending.Operation, pending.Target, pending.Summary, pending.PayloadHash, "succeeded", pending.ID); err != nil {
			return nil, errors.New("operation succeeded but audit write failed")
		}
		return map[string]any{"approval": decided, "result": result}, nil
	})
}

func (a *App) mcpApprovalAllowed(ctx context.Context, agent mcpcontrol.Agent, approval mcpcontrol.Approval, tokenScopes []string) bool {
	if agent.Role != "operator" && agent.Role != "admin" {
		return false
	}
	scope := ""
	switch approval.Operation {
	case "groups.remove":
		scope = "group.write"
	case "queue.retry_all":
		scope = "queue.retry_all"
	case "message.send":
		scope = "message.send"
	case "message.edit":
		scope = "message.edit"
	case "topic.create", "topic.update", "topic.bulk_create", "topic.bulk_update":
		scope = "topic.write"
	default:
		return false
	}
	hasTokenScope := false
	for _, granted := range tokenScopes {
		if granted == scope {
			hasTokenScope = true
			break
		}
	}
	if !agent.AllowsScope(scope) || !hasTokenScope {
		return false
	}
	if approval.Operation == "groups.remove" {
		return agent.AllowsChat(approval.Target) && a.mcpChatAccountsAllowed(ctx, agent, approval.Target)
	}
	if approval.Operation != "queue.retry_all" {
		var payload struct {
			AccountID string `json:"accountId"`
		}
		return agent.AllowsChat(approval.Target) && json.Unmarshal(approval.Payload, &payload) == nil && a.mcpChatAccountAllowed(ctx, agent, approval.Target, payload.AccountID)
	}
	var payload struct {
		Keys []string `json:"keys"`
	}
	if json.Unmarshal(approval.Payload, &payload) != nil || len(payload.Keys) == 0 || len(payload.Keys) > 100 {
		return false
	}
	for _, key := range payload.Keys {
		group, account, err := a.mcpQueueTarget(ctx, key)
		if err != nil || !agent.AllowsChat(group) || !agent.AllowsAccount(account) || !a.mcpChatAccountsAllowed(ctx, agent, group) {
			return false
		}
	}
	return true
}

func addMCPMapTool[In any](server *mcp.Server, auditStore *mcpcontrol.Store, name, description string, readOnly bool, handler func(context.Context, In) (map[string]any, error)) {
	annotations := &mcp.ToolAnnotations{Title: name, ReadOnlyHint: readOnly, IdempotentHint: readOnly}
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, Annotations: annotations}, func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, map[string]any, error) {
		out, err := handler(ctx, input)
		if err != nil {
			if info := sdkauth.TokenInfoFromContext(ctx); info != nil && info.UserID != "" {
				if encoded, marshalErr := json.Marshal(input); marshalErr == nil {
					_ = auditStore.Audit(context.WithoutCancel(ctx), "agent", info.UserID, name, "", "tool call failed", mcpcontrol.PayloadDigest(json.RawMessage(encoded)), "failed", "")
				}
			}
		}
		return nil, out, err
	})
}

func (a *App) requireMCPAgent(ctx context.Context, scope string) (mcpcontrol.Agent, []string, error) {
	info := sdkauth.TokenInfoFromContext(ctx)
	if info == nil || info.UserID == "" {
		return mcpcontrol.Agent{}, nil, errors.New("authenticated MCP agent is required")
	}
	agent, err := a.mcpControl.Agent(ctx, info.UserID)
	if err != nil || !agent.Active {
		return mcpcontrol.Agent{}, nil, errors.New("MCP agent is revoked or unavailable")
	}
	allowed := map[string]bool{}
	for _, value := range info.Scopes {
		allowed[value] = true
	}
	if scope != "" && (!agent.AllowsScope(scope) || !allowed[scope]) {
		return mcpcontrol.Agent{}, nil, fmt.Errorf("agent lacks %s permission", scope)
	}
	return agent, info.Scopes, nil
}

func tokenHasScope(scopes []string, scope string) bool {
	for _, value := range scopes {
		if value == scope {
			return true
		}
	}
	return false
}

func (a *App) mcpChatAccountAllowed(ctx context.Context, agent mcpcontrol.Agent, chatID, accountID string) bool {
	if chatID == "" || accountID == "" || !agent.AllowsChat(chatID) || !agent.AllowsAccount(accountID) || !a.mcpChatAccountsAllowed(ctx, agent, chatID) {
		return false
	}
	var raw sql.NullString
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT accounts FROM chat_access WHERE chat_id=?`, chatID).Scan(&raw); err != nil || !raw.Valid {
		return false
	}
	var accounts map[string]json.RawMessage
	if json.Unmarshal([]byte(raw.String), &accounts) != nil {
		return false
	}
	_, ok := accounts[accountID]
	return ok
}

func (a *App) mcpRequestApproval(ctx context.Context, agent mcpcontrol.Agent, key, operation, target, summary string, payload any) (map[string]any, error) {
	hash := mcpcontrol.PayloadDigest(payload)
	if err := a.mcpControl.Audit(ctx, "agent", agent.ID, operation, target, "requested approved Telegram operation", hash, "requested", ""); err != nil {
		return nil, errors.New("audit unavailable; operation was not queued")
	}
	if cached, hit, err := a.mcpControl.BeginIdempotency(ctx, agent.ID, key, payload); err != nil {
		return nil, err
	} else if hit {
		var out map[string]any
		_ = json.Unmarshal(cached, &out)
		return out, nil
	}
	approval, err := a.mcpControl.CreateApproval(ctx, agent.ID, operation, target, summary, payload)
	if err != nil {
		return nil, err
	}
	base := map[string]any{"status": "approval_required", "approvalId": approval.ID, "expiresAt": approval.ExpiresAt, "operation": approval.Operation, "target": approval.Target}
	if err := a.mcpControl.CompleteIdempotency(ctx, agent.ID, key, base); err != nil {
		return nil, err
	}
	if err := a.mcpControl.Audit(ctx, "agent", agent.ID, operation, target, summary, approval.PayloadHash, "pending_approval", approval.ID); err != nil {
		return nil, errors.New("approval is pending but audit linkage failed")
	}
	base["preview"] = payload
	return base, nil
}

func validMCPTopicTitle(value string) bool {
	value = strings.TrimSpace(value)
	return utf8.ValidString(value) && len(value) > 0 && len(value) <= 128
}

func validMCPMessage(value string) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	units := 0
	for _, char := range value {
		if char > 0xFFFF {
			units += 2
		} else {
			units++
		}
		if units > 4096 {
			return false
		}
	}
	return true
}

func (a *App) mcpChatAccountsAllowed(ctx context.Context, agent mcpcontrol.Agent, chatID string) bool {
	if agent.AllowsAccount("*") {
		return true
	}
	var raw sql.NullString
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT accounts FROM chat_access WHERE chat_id=?`, chatID).Scan(&raw); err != nil || !raw.Valid {
		return false
	}
	var accounts map[string]json.RawMessage
	if json.Unmarshal([]byte(raw.String), &accounts) != nil || len(accounts) == 0 {
		return false
	}
	count := 0
	for id := range accounts {
		if id == "_" {
			continue
		}
		count++
		if !agent.AllowsAccount(id) {
			return false
		}
	}
	return count > 0
}

func validateMCPGroupPatch(agent mcpcontrol.Agent, patch map[string]any) error {
	for field := range patch {
		switch field {
		case "name", "enabled", "filters", "autoForward", "trackUsers", "topics", "rescueMode", "rescueRetentionHours", "confirmForwardReroute":
		default:
			return fmt.Errorf("group configuration field %q is not exposed to MCP", field)
		}
	}
	for _, field := range []string{"autoForward", "forwarding", "routes"} {
		if raw, ok := patch[field]; ok {
			if err := validateMCPDestinationScope(agent, raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func filterMCPGroup(group map[string]any, agent mcpcontrol.Agent) map[string]any {
	var filter func(any, string) any
	filter = func(value any, parent string) any {
		switch item := value.(type) {
		case map[string]any:
			for key, child := range item {
				item[key] = filter(child, key)
			}
			return item
		case []any:
			out := make([]any, 0, len(item))
			for _, child := range item {
				if filtered := filter(child, parent); filtered != nil {
					out = append(out, filtered)
				}
			}
			return out
		default:
			id := toString(value)
			if (strings.Contains(strings.ToLower(parent), "destination") || parent == "chatId" || parent == "groupId") && id != "" && id != "saved-messages" && !agent.AllowsChat(id) {
				return nil
			}
		}
		return value
	}
	filtered, _ := filter(group, "").(map[string]any)
	return filtered
}

func validateMCPDestinationScope(agent mcpcontrol.Agent, raw any) error {
	switch value := raw.(type) {
	case map[string]any:
		for key, child := range value {
			if strings.Contains(strings.ToLower(key), "destination") || key == "chatId" || key == "groupId" {
				if id := toString(child); id != "" && id != "saved-messages" && !agent.AllowsChat(id) {
					return errors.New("agent is not allowed to route to this chat")
				}
			}
			if err := validateMCPDestinationScope(agent, child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := validateMCPDestinationScope(agent, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) mcpQueueTarget(ctx context.Context, key string) (string, string, error) {
	group, message, ok := strings.Cut(key, "_")
	if !ok || group == "" || message == "" {
		return "", "", errors.New("queue key must use groupId_messageId format")
	}
	var account string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT account_id FROM tgdl_work WHERE group_id=? AND message_id=?`, group, message).Scan(&account); err != nil {
		return "", "", err
	}
	return group, account, nil
}

func (a *App) mcpFailedQueueKeys(ctx context.Context, agent mcpcontrol.Agent, limit int) ([]string, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id,message_id,account_id FROM tgdl_work WHERE status IN ('failed','interrupted') ORDER BY updated_at DESC LIMIT ?`, limit*20)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var group, account string
		var message int64
		if err := rows.Scan(&group, &message, &account); err != nil {
			return nil, err
		}
		if agent.AllowsChat(group) && agent.AllowsAccount(account) && a.mcpChatAccountAllowed(ctx, agent, group, account) {
			keys = append(keys, fmt.Sprintf("%s_%d", group, message))
			if len(keys) >= limit {
				break
			}
		}
	}
	return keys, rows.Err()
}

func (a *App) executeMCPApproval(ctx context.Context, approval mcpcontrol.Approval) (map[string]any, error) {
	if err := a.validateMCPApprovalRequester(ctx, approval); err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(approval.Payload, &payload); err != nil {
		return nil, err
	}
	switch approval.Operation {
	case "groups.remove":
		id := toString(payload["groupId"])
		a.configMu.Lock()
		defer a.configMu.Unlock()
		cfg, err := a.config.Load(ctx)
		if err != nil {
			return nil, err
		}
		groups, _ := cfg["groups"].([]any)
		kept := make([]any, 0, len(groups))
		removed := false
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if ok && toString(group["id"]) == id {
				removed = true
				continue
			}
			kept = append(kept, raw)
		}
		if !removed {
			return nil, sql.ErrNoRows
		}
		cfg["groups"] = kept
		if err := a.config.Save(ctx, cfg); err != nil {
			return nil, err
		}
		a.hub.Broadcast(ws.Event{Type: "config_updated"})
		return map[string]any{"success": true, "removedGroupId": id}, nil
	case "queue.retry_all":
		raw, _ := payload["keys"].([]any)
		results := make([]map[string]any, 0, len(raw))
		for _, v := range raw {
			key, ok := v.(string)
			if !ok {
				continue
			}
			if _, _, err := a.mcpQueueTarget(ctx, key); err != nil {
				results = append(results, map[string]any{"key": key, "status": "skipped", "reason": "item no longer exists"})
				continue
			}
			out, err := callMCPAppHandler(ctx, "POST", "/api/queue/"+key+"/retry", map[string]string{"key": key}, nil, a.handleQueueRetry)
			if err != nil {
				results = append(results, map[string]any{"key": key, "status": "failed", "error": err.Error()})
			} else {
				results = append(results, map[string]any{"key": key, "status": "retried", "result": out})
			}
		}
		return map[string]any{"status": mcpOutcomeStatus(results), "items": results}, nil
	case "message.send", "message.edit", "topic.create", "topic.update", "topic.bulk_create", "topic.bulk_update":
		return a.executeMCPChatApproval(ctx, approval, payload)
	default:
		return nil, fmt.Errorf("unsupported approved operation %q", approval.Operation)
	}
}

func (a *App) validateMCPApprovalRequester(ctx context.Context, approval mcpcontrol.Approval) error {
	requester, err := a.mcpControl.Agent(ctx, approval.RequesterID)
	if err != nil || !requester.Active {
		return errors.New("requesting agent was revoked before approval")
	}
	scope := map[string]string{
		"groups.remove": "group.write", "queue.retry_all": "queue.retry_all",
		"message.send": "message.send", "message.edit": "message.edit",
		"topic.create": "topic.write", "topic.update": "topic.write",
		"topic.bulk_create": "topic.write", "topic.bulk_update": "topic.write",
	}[approval.Operation]
	if scope == "" || !requester.AllowsScope(scope) {
		return errors.New("requesting agent no longer has this operation scope")
	}
	if approval.Operation == "queue.retry_all" {
		var payload struct {
			Keys []string `json:"keys"`
		}
		if json.Unmarshal(approval.Payload, &payload) != nil || len(payload.Keys) == 0 || len(payload.Keys) > 100 {
			return errors.New("invalid retry-all approval payload")
		}
		for _, key := range payload.Keys {
			group, account, err := a.mcpQueueTarget(ctx, key)
			if err != nil || !requester.AllowsChat(group) || !requester.AllowsAccount(account) || !a.mcpChatAccountAllowed(ctx, requester, group, account) {
				return errors.New("requesting agent no longer has access to every queue item")
			}
		}
		return nil
	}
	if approval.Operation == "groups.remove" {
		if !requester.AllowsChat(approval.Target) || !a.mcpChatAccountsAllowed(ctx, requester, approval.Target) {
			return errors.New("requesting agent no longer has access to this chat")
		}
		return nil
	}
	var payload struct {
		AccountID string `json:"accountId"`
	}
	if json.Unmarshal(approval.Payload, &payload) != nil || !a.mcpChatAccountAllowed(ctx, requester, approval.Target, payload.AccountID) {
		return errors.New("requesting agent no longer has access to this chat/account")
	}
	return nil
}

func (a *App) executeMCPChatApproval(ctx context.Context, approval mcpcontrol.Approval, payload map[string]any) (map[string]any, error) {
	accountID, groupID := toString(payload["accountId"]), toString(payload["groupId"])
	session, err := a.monitor.OpenDialogs(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	opCtx := session.Context()
	account, err := session.ChatAccount(opCtx, accountID)
	if err != nil {
		return nil, err
	}
	switch approval.Operation {
	case "message.send":
		messageID, err := account.SendText(opCtx, groupID, int(number(payload["topicId"], 0)), toString(payload["message"]))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "executed", "messageId": messageID, "groupId": groupID}, nil
	case "message.edit":
		err := account.EditText(opCtx, groupID, int(number(payload["messageId"], 0)), toString(payload["message"]))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "executed", "messageId": int(number(payload["messageId"], 0)), "groupId": groupID}, nil
	case "topic.create":
		topic, err := account.CreateForumTopic(opCtx, groupID, toString(payload["title"]))
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "executed", "topic": topic}, nil
	case "topic.update", "topic.bulk_update":
		raw, ok := payload["updates"].([]any)
		if !ok || len(raw) == 0 || len(raw) > 50 {
			return nil, errors.New("invalid stored topic update payload")
		}
		results := make([]map[string]any, 0, len(raw))
		for index, value := range raw {
			encoded, _ := json.Marshal(value)
			var update mcpTopicUpdate
			if err := json.Unmarshal(encoded, &update); err != nil {
				results = append(results, map[string]any{"index": index, "status": "failed", "error": "invalid update"})
				continue
			}
			err := account.UpdateForumTopic(opCtx, groupID, update.TopicID, update.Title, update.Closed, update.IconEmojiID, update.Hidden)
			if err != nil {
				results = append(results, map[string]any{"index": index, "topicId": update.TopicID, "status": "failed", "error": err.Error()})
			} else {
				results = append(results, map[string]any{"index": index, "topicId": update.TopicID, "status": "updated"})
			}
		}
		return map[string]any{"status": mcpOutcomeStatus(results), "items": results}, nil
	case "topic.bulk_create":
		raw, ok := payload["titles"].([]any)
		if !ok || len(raw) == 0 || len(raw) > 50 {
			return nil, errors.New("invalid stored bulk topic payload")
		}
		results := make([]map[string]any, 0, len(raw))
		for index, value := range raw {
			title, ok := value.(string)
			if !ok {
				results = append(results, map[string]any{"index": index, "status": "failed", "error": "invalid title"})
				continue
			}
			topic, err := account.CreateForumTopic(opCtx, groupID, title)
			if err != nil {
				results = append(results, map[string]any{"index": index, "title": title, "status": "failed", "error": err.Error()})
			} else {
				results = append(results, map[string]any{"index": index, "title": title, "status": "created", "topic": topic})
			}
		}
		return map[string]any{"status": mcpOutcomeStatus(results), "items": results}, nil
	default:
		return nil, fmt.Errorf("unsupported chat approval operation %q", approval.Operation)
	}
}

func mcpOutcomeStatus(items []map[string]any) string {
	for _, item := range items {
		switch item["status"] {
		case "retried", "created", "updated":
		default:
			return "partial"
		}
	}
	return "executed"
}

func mcpApprovalResultStatus(result map[string]any) string {
	if result["status"] == "partial" {
		return "partial"
	}
	return "executed"
}

func hasMCPTopicChange(title *string, closed *bool, icon *int64, hidden *bool) bool {
	return title != nil || closed != nil || icon != nil || hidden != nil
}

func callMCPAppHandler(ctx context.Context, method, path string, pathValues map[string]string, body any, handler http.HandlerFunc) (map[string]any, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range pathValues {
		req.SetPathValue(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code < 200 || recorder.Code >= 300 {
		return nil, fmt.Errorf("application operation failed (%d): %s", recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	var result map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("decode application response: %w", err)
	}
	return result, nil
}

func (a *App) filterQueueSnapshot(ctx context.Context, snapshot map[string]any, agent mcpcontrol.Agent) map[string]any {
	out := map[string]any{"engineRunning": snapshot["engineRunning"], "globalPaused": snapshot["globalPaused"]}
	for _, section := range []string{"active", "queued", "recent"} {
		raw, _ := snapshot[section].([]map[string]any)
		items := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			group := toString(item["groupId"])
			account := toString(item["accountId"])
			if !agent.AllowsChat(group) || !agent.AllowsAccount(account) || !a.mcpChatAccountAllowed(ctx, agent, group, account) {
				continue
			}
			copy := cloneConfigValue(item).(map[string]any)
			delete(copy, "filePath")
			items = append(items, copy)
		}
		out[section] = items
	}
	out["pending"] = len(out["queued"].([]map[string]any))
	out["visibleActive"] = len(out["active"].([]map[string]any))
	return out
}

func (a *App) filterForwardSnapshot(ctx context.Context, snapshot map[string]any, agent mcpcontrol.Agent) map[string]any {
	itemsRaw, _ := snapshot["items"].([]map[string]any)
	items := make([]map[string]any, 0, len(itemsRaw))
	counts := map[string]int{"pending": 0, "processing": 0, "forwarded": 0, "failed": 0, "interrupted": 0}
	for _, item := range itemsRaw {
		group := toString(item["groupId"])
		if !agent.AllowsChat(group) || !a.mcpChatAccountsAllowed(ctx, agent, group) {
			continue
		}
		filtered := cloneConfigValue(item).(map[string]any)
		var destinations []any
		allowedDestinations := map[string]bool{}
		if raw, ok := filtered["destinations"].([]string); ok {
			for _, destination := range raw {
				if agent.AllowsChat(destination) && a.mcpChatAccountsAllowed(ctx, agent, destination) {
					destinations = append(destinations, destination)
					allowedDestinations[destination] = true
				}
			}
		}
		filtered["destinations"] = destinations
		if states, ok := filtered["destinationStates"].([]forwardDestination); ok {
			visibleStates := make([]forwardDestination, 0, len(states))
			for _, state := range states {
				destination := toString(state.Settings["destinationGroupId"])
				if destination == "" {
					destination = toString(state.Settings["destination"])
				}
				if allowedDestinations[destination] {
					state.Settings = map[string]any{"destinationGroupId": destination}
					visibleStates = append(visibleStates, state)
				}
			}
			filtered["destinationStates"] = visibleStates
		} else {
			delete(filtered, "destinationStates")
		}
		if len(destinations) == 0 && toString(item["destinations"]) != "[]" {
			continue
		}
		items = append(items, filtered)
		state := toString(item["status"])
		counts[state]++
	}
	return map[string]any{"total": len(items), "pending": counts["pending"], "active": counts["processing"], "forwarded": counts["forwarded"], "failed": counts["failed"], "interrupted": counts["interrupted"], "items": items}
}
