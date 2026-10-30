package app

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/botnick/telegram-media-downloader/core-service/internal/mcpcontrol"
)

func registerMCPAdminRoutes(mux *http.ServeMux, a *App) {
	mux.Handle("GET /api/mcp/agents", a.requireAdmin(http.HandlerFunc(a.handleMCPAgents)))
	mux.Handle("POST /api/mcp/agents", a.requireAdmin(http.HandlerFunc(a.handleMCPAgentCreate)))
	mux.Handle("PATCH /api/mcp/agents/{id}", a.requireAdmin(http.HandlerFunc(a.handleMCPAgentUpdate)))
	mux.Handle("DELETE /api/mcp/agents/{id}", a.requireAdmin(http.HandlerFunc(a.handleMCPAgentRevoke)))
	mux.Handle("POST /api/mcp/agents/{id}/rotate-secret", a.requireAdmin(http.HandlerFunc(a.handleMCPAgentRotate)))
	mux.Handle("GET /api/mcp/approvals", a.requireAdmin(http.HandlerFunc(a.handleMCPApprovals)))
	mux.Handle("POST /api/mcp/approvals/{id}/decision", a.requireAdmin(http.HandlerFunc(a.handleMCPApprovalDecision)))
	mux.Handle("GET /api/mcp/audit", a.requireAdmin(http.HandlerFunc(a.handleMCPAudit)))
}

func (a *App) handleMCPAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := a.mcpControl.ListAgents(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "agent list failed")
		return
	}
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.list", "", "listed MCP agent identities", "", "succeeded", ""); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents, "roles": mcpcontrol.Presets()})
}

func (a *App) handleMCPAgentCreate(w http.ResponseWriter, r *http.Request) {
	var in mcpcontrol.AgentInput
	if decodeBody(w, r, &in) != nil {
		return
	}
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.create", "", "requested MCP agent creation", "", "requested", ""); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable; agent was not created")
		return
	}
	agent, secret, err := a.mcpControl.CreateAgent(r.Context(), in)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditWarning := ""
	if err = a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.create", agent.ID, "created MCP agent credentials", "", "succeeded", ""); err != nil {
		auditWarning = "completion audit failed; request event is retained"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"agent": agent, "clientSecret": secret, "auditWarning": auditWarning})
}

func (a *App) handleMCPAgentUpdate(w http.ResponseWriter, r *http.Request) {
	var in mcpcontrol.AgentInput
	if decodeBody(w, r, &in) != nil {
		return
	}
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.update", r.PathValue("id"), "requested agent permission update", "", "requested", ""); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable; agent was not updated")
		return
	}
	agent, err := a.mcpControl.UpdateAgent(r.Context(), r.PathValue("id"), in)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "agent update failed: "+err.Error())
		return
	}
	auditWarning := ""
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.update", agent.ID, "updated agent permissions", "", "succeeded", ""); err != nil {
		auditWarning = "completion audit failed; request event is retained"
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent, "auditWarning": auditWarning})
}

func (a *App) handleMCPAgentRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.revoke", id, "requested agent revocation", "", "requested", ""); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable; agent was not revoked")
		return
	}
	if err := a.mcpControl.RevokeAgent(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusNotFound, "agent not found or already revoked")
		return
	}
	auditWarning := ""
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.revoke", id, "revoked agent and active tokens", "", "succeeded", ""); err != nil {
		auditWarning = "completion audit failed; request event is retained"
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "auditWarning": auditWarning})
}

func (a *App) handleMCPAgentRotate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.rotate_secret", id, "requested client secret rotation", "", "requested", ""); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable; secret was not rotated")
		return
	}
	secret, err := a.mcpControl.RotateSecret(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "agent not found or inactive")
		return
	}
	auditWarning := ""
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "agent.rotate_secret", id, "rotated client secret and revoked active tokens", "", "succeeded", ""); err != nil {
		auditWarning = "completion audit failed; request event is retained"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"clientSecret": secret, "auditWarning": auditWarning})
}

func (a *App) handleMCPApprovals(w http.ResponseWriter, r *http.Request) {
	pending := r.URL.Query().Get("pending") != "false"
	items, err := a.mcpControl.ListApprovals(r.Context(), pending)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "approval list failed")
		return
	}
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "approval.list", "", "read MCP operation approvals", "", "succeeded", ""); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": items})
}

func (a *App) handleMCPApprovalDecision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Decision string `json:"decision"`
	}
	if decodeBody(w, r, &in) != nil {
		return
	}
	if err := a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "approval."+in.Decision, r.PathValue("id"), "requested approval decision", "", "requested", r.PathValue("id")); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable; approval was not decided")
		return
	}
	item, err := a.mcpControl.DecideApproval(r.Context(), r.PathValue("id"), in.Decision, "dashboard-admin")
	if err != nil {
		writeJSONError(w, http.StatusConflict, "approval is missing, expired, or already decided")
		return
	}
	if in.Decision == "rejected" {
		_ = a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", "approval.reject", item.Target, "rejected pending operation", item.PayloadHash, "rejected", item.ID)
		writeJSON(w, http.StatusOK, map[string]any{"approval": item})
		return
	}
	result, executeErr := a.executeMCPApproval(r.Context(), item)
	if executeErr != nil {
		_ = a.mcpControl.CompleteApproval(r.Context(), item.ID, "failed", map[string]string{"error": executeErr.Error()})
		_ = a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", item.Operation, item.Target, item.Summary, item.PayloadHash, "failed", item.ID)
		writeJSONError(w, http.StatusBadGateway, "approved operation failed: "+executeErr.Error())
		return
	}
	_ = a.mcpControl.CompleteApproval(r.Context(), item.ID, mcpApprovalResultStatus(result), result)
	_ = a.mcpControl.Audit(r.Context(), "operator", "dashboard-admin", item.Operation, item.Target, item.Summary, item.PayloadHash, "succeeded", item.ID)
	writeJSON(w, http.StatusOK, map[string]any{"approval": item, "result": result})
}

func (a *App) handleMCPAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := a.mcpControl.AuditEntries(r.Context(), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "audit read failed")
		return
	}
	if r.URL.Query().Get("format") == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, item := range items {
			encoded, _ := json.Marshal(item)
			_, _ = w.Write(append(encoded, '\n'))
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": items})
}
