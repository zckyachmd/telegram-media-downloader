package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Auto-forward, as 2.x did it: after a chat's file is downloaded, the file is
// uploaded to the chat's "Forward to" destination with a caption linking the
// source post. deleteAfterForward then removes the local copy (never anything
// on Telegram). Forwards use a separate concurrency limit and never hold up
// the download queue.

// forwardSender is the engine surface auto-forward needs.
type forwardSender interface {
	Observing() bool
	SendFile(context.Context, string, telegram.SendFileRequest) (string, int, error)
}
type nativeForwardSender interface {
	ForwardMessage(context.Context, string, telegram.ForwardMessageRequest) (string, int, error)
}

type forwardJob struct {
	queueID            int64
	groupID, groupName string
	fileName           string
	messageID          int
	topicID            int
	text               string
	recordID           int64
	historyID          int64
	path               string
	facts              telegram.MediaFacts
	settings           map[string]any
	accountID          string
	stage              string
	destinations       []forwardDestination
	attempts           int
	directForward      func(context.Context, telegram.ForwardMessageRequest) error
}

type forwardDestination struct {
	Settings             map[string]any `json:"settings"`
	Status               string         `json:"status"`
	Method               string         `json:"method"`
	Reason               string         `json:"reason,omitempty"`
	DirectFailed         bool           `json:"directFailed,omitempty"`
	DestinationMessageID int            `json:"destinationMessageId,omitempty"`
}

type forwardConfigSnapshot struct {
	Settings     map[string]any       `json:"settings"`
	AccountID    string               `json:"accountId"`
	Destinations []forwardDestination `json:"destinations,omitempty"`
}

func (a *App) handleForwardProtection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Protect bool `json:"protect"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	cfg, err := a.config.Load(r.Context())
	if err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	forwarding, _ := cfg["forwarding"].(map[string]any)
	if forwarding == nil {
		forwarding = map[string]any{}
	}
	forwarding["protect"] = body.Protect
	cfg["forwarding"] = forwarding
	if err := a.config.Save(r.Context(), cfg); err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}

const (
	maxForwardConcurrency = 5
)

// queueForward persists the job before waking the single forward worker.
func (a *App) queueForward(job forwardJob) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if job.settings == nil {
		cfg, err := a.config.Load(ctx)
		if err != nil {
			a.forwardFailed(job, "", err)
			return
		}
		job.settings, job.accountID, _ = forwardSettings(cfg, job.groupID)
	}
	if job.stage == "" {
		job.stage = "metadata"
	}
	facts, err := json.Marshal(job.facts)
	if err != nil {
		a.forwardFailed(job, "", err)
		return
	}
	globalConfig, _ := a.config.Load(ctx)
	resolved := job.destinations
	if len(resolved) == 0 {
		for _, route := range matchingForwardRoutes(job.settings, job.topicID) {
			settings := effectiveForwardProtection(inheritCaptionSettings(route, job.settings), job.settings, route, job.topicID, globalConfig)
			resolved = append(resolved, forwardDestination{Settings: settings, Status: "pending"})
		}
	}
	forwardConfig, err := json.Marshal(forwardConfigSnapshot{Settings: job.settings, AccountID: job.accountID, Destinations: resolved})
	if err != nil {
		a.forwardFailed(job, "", err)
		return
	}
	queueLimit := a.forwarderConfig(ctx).queueSize
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		a.forwardFailed(job, "", err)
		return
	}
	defer tx.Rollback()
	var queued int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_forward_queue WHERE status IN ('pending','processing')`).Scan(&queued); err != nil {
		_ = tx.Rollback()
		a.forwardFailed(job, "", err)
		return
	}
	if queued >= queueLimit {
		now := time.Now().UnixMilli()
		reason := "auto-forward queue is full"
		err = tx.QueryRowContext(ctx, `INSERT INTO tgdl_forward_queue(history_id,group_id,group_name,message_id,file_name,topic_id,caption,record_id,file_path,facts_json,forward_config_json,status,reason,stage,created_at,updated_at,completed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,'failed',?,'enqueue',?,?,?) RETURNING id`, job.historyID, job.groupID, job.groupName, job.messageID, job.topicID, job.text, job.recordID, job.path, string(facts), string(forwardConfig), reason, now, now, now).Scan(&job.queueID)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			a.forwardFailed(job, "", err)
			return
		}
		a.forwardFailed(job, "", errors.New(reason))
		a.notifyForwardQueue()
		return
	}
	now := time.Now().UnixMilli()
	if job.fileName == "" && job.path != "" {
		job.fileName = path.Base(job.path)
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO tgdl_forward_queue(history_id,group_id,group_name,message_id,file_name,topic_id,caption,record_id,file_path,facts_json,forward_config_json,stage,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`, job.historyID, job.groupID, job.groupName, job.messageID, job.fileName, job.topicID, job.text, job.recordID, job.path, string(facts), string(forwardConfig), job.stage, now, now).Scan(&job.queueID)
	if err != nil {
		_ = tx.Rollback()
		a.forwardFailed(job, "", err)
		return
	}
	if err = tx.Commit(); err != nil {
		a.forwardFailed(job, "", err)
		return
	}
	a.notifyForwardQueue()
	select {
	case a.forwards <- struct{}{}:
	default:
	}
}

func (a *App) reserveForward(job *forwardJob) bool {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	facts, err := json.Marshal(job.facts)
	if err != nil {
		return false
	}
	forwardConfig, err := json.Marshal(forwardConfigSnapshot{Settings: job.settings, AccountID: job.accountID, Destinations: job.destinations})
	if err != nil {
		return false
	}
	limit := a.forwarderConfig(ctx).queueSize
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	var queued int
	if tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_forward_queue WHERE status IN ('pending','processing')`).Scan(&queued) != nil || queued >= limit {
		return false
	}
	now := time.Now().UnixMilli()
	if tx.QueryRowContext(ctx, `INSERT INTO tgdl_forward_queue(history_id,group_id,group_name,message_id,file_name,topic_id,caption,record_id,file_path,facts_json,forward_config_json,status,stage,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,'processing',?,?,?) RETURNING id`, job.historyID, job.groupID, job.groupName, job.messageID, job.fileName, job.topicID, job.text, 0, "", string(facts), string(forwardConfig), job.stage, now, now).Scan(&job.queueID) != nil {
		return false
	}
	if tx.Commit() != nil {
		return false
	}
	job.attempts = 1
	return job.queueID > 0
}

func (a *App) updateForwardReservation(ctx context.Context, job forwardJob, status, reason string) {
	now := time.Now().UnixMilli()
	completedAt := now
	if status == "pending" || status == "processing" {
		completedAt = 0
	}
	_, _ = a.db.Writer.ExecContext(ctx, `UPDATE tgdl_forward_queue SET status=?,reason=?,stage=?,record_id=?,file_path=?,destinations_json=?,updated_at=?,completed_at=? WHERE history_id=?`, status, reason, job.stage, job.recordID, job.path, mustJSON(job.destinations), now, completedAt, job.historyID)
	a.notifyForwardQueue()
}

func mustJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }

func (a *App) directForwardNow(ctx context.Context, job *forwardJob) error {
	for i, route := range matchingForwardRoutes(job.settings, job.topicID) {
		settings := job.destinations[i].Settings
		destination := toString(settings["destinationGroupId"])
		if destination == "" {
			destination = toString(settings["destination"])
		}
		if job.directForward == nil {
			return errors.New("native Telegram forwarding is unavailable")
		}
		request := telegram.ForwardMessageRequest{Source: job.groupID, Destination: destination, MessageID: job.messageID, TopicID: int(number(route["destinationTopicId"], 0))}
		var destinationMessageID int
		request.ResultMessageID = &destinationMessageID
		if toString(settings["captionMode"]) == "custom" {
			caption, entities, err := routeCaption(settings, *job)
			if err != nil {
				return err
			}
			request.EditCaption, request.Caption, request.Entities = true, caption, entities
		}
		job.destinations[i].Status, job.destinations[i].Method = "processing", "native_forward"
		a.persistForwardProgress(ctx, *job)
		forwardErr := job.directForward(ctx, request)
		job.destinations[i].DestinationMessageID = destinationMessageID
		if forwardErr != nil {
			job.destinations[i].DirectFailed = true
			job.destinations[i].Reason = "direct forward failed"
			if errors.Is(forwardErr, telegram.ErrForwardCaptionUnresolved) {
				job.stage = "caption_edit_failed"
			} else {
				job.stage = "fallback_download"
			}
			a.persistForwardProgress(ctx, *job)
			return forwardErr
		}
		job.destinations[i].DirectFailed = false
		job.destinations[i].Reason = ""
		job.destinations[i].Status = "forwarded"
		a.persistForwardProgress(ctx, *job)
	}
	return nil
}

func (a *App) runForwards(ctx context.Context) {
	var active atomic.Int32
	var workers sync.WaitGroup
	var scheduleMu sync.Mutex
	var nextStart time.Time
	for range maxForwardConcurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				config := a.forwarderConfig(ctx)
				current := active.Load()
				if current < int32(config.concurrency) && active.CompareAndSwap(current, current+1) {
					job, err := a.claimForward(ctx)
					if err == nil && job != nil {
						if a.waitForwardStart(ctx, &scheduleMu, &nextStart, time.Duration(config.intervalSec)*time.Second) {
							a.notifyForwardQueue()
						}
						a.forward(ctx, *job)
						active.Add(-1)
						continue
					}
					active.Add(-1)
					if err != nil && ctx.Err() == nil && a.output != nil {
						fmt.Fprintf(a.output, "AutoForward: could not claim queued job: %v\n", err)
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-a.forwards:
				case <-ticker.C:
				}
			}
		}()
	}
	<-ctx.Done()
	workers.Wait()
}

type forwarderRuntimeConfig struct {
	concurrency, queueSize, intervalSec, floodWaitAttempts int
}

func (a *App) forwarderConfig(ctx context.Context) forwarderRuntimeConfig {
	cfg, err := a.config.Load(ctx)
	if err != nil {
		return forwarderRuntimeConfig{concurrency: 1, queueSize: 256, floodWaitAttempts: 3}
	}
	advanced, _ := cfg["advanced"].(map[string]any)
	forwarder, _ := advanced["forwarder"].(map[string]any)
	return forwarderRuntimeConfig{
		concurrency:       max(1, min(maxForwardConcurrency, int(number(forwarder["concurrency"], 1)))),
		queueSize:         max(1, min(10000, int(number(forwarder["queueSize"], 256)))),
		intervalSec:       max(0, min(3600, int(number(forwarder["intervalSec"], 0)))),
		floodWaitAttempts: max(1, min(10, int(number(forwarder["floodWaitAttempts"], 3)))),
	}
}

func (a *App) waitForwardStart(ctx context.Context, mu *sync.Mutex, next *time.Time, interval time.Duration) bool {
	mu.Lock()
	start := time.Now()
	if next.After(start) {
		start = *next
	}
	*next = start.Add(interval)
	mu.Unlock()
	if wait := time.Until(start); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	return ctx.Err() == nil
}

func (a *App) claimForward(ctx context.Context) (*forwardJob, error) {
	var job forwardJob
	var facts, forwardConfig, destinations string
	err := a.db.Writer.QueryRowContext(ctx, `UPDATE tgdl_forward_queue SET status='processing',updated_at=? WHERE id=(
		SELECT id FROM tgdl_forward_queue WHERE status='pending' AND COALESCE((SELECT paused FROM tgdl_queue_state WHERE id=1),0)=0 ORDER BY id LIMIT 1
	) RETURNING id,history_id,group_id,group_name,message_id,file_name,topic_id,caption,record_id,file_path,facts_json,forward_config_json,stage,destinations_json,attempts`, time.Now().UnixMilli()).Scan(
		&job.queueID, &job.historyID, &job.groupID, &job.groupName, &job.messageID, &job.fileName, &job.topicID, &job.text, &job.recordID, &job.path, &facts, &forwardConfig, &job.stage, &destinations, &job.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(facts), &job.facts); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(destinations), &job.destinations)
	if forwardConfig != "" {
		var snapshot forwardConfigSnapshot
		if err = json.Unmarshal([]byte(forwardConfig), &snapshot); err != nil {
			return nil, err
		}
		job.settings, job.accountID = snapshot.Settings, snapshot.AccountID
		if len(snapshot.Destinations) > 0 && len(job.destinations) == 0 {
			job.destinations = snapshot.Destinations
		}
	}
	return &job, nil
}

func (a *App) notifyForwardQueue() {
	if a.hub != nil {
		a.hub.Broadcast(ws.Event{Type: "forward_queue_changed", Flat: true})
	}
}

func (a *App) handleManualForwardEnqueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs         []int64  `json:"ids"`
		Paths       []string `json:"paths"`
		Destination string   `json:"destination"`
		Caption     string   `json:"caption"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	if len(body.IDs) == 0 && len(body.Paths) > 0 {
		for _, p := range body.Paths {
			var id int64
			if err := a.db.Reader.QueryRowContext(r.Context(), `SELECT id FROM downloads WHERE REPLACE(file_path,char(92),'/')=? AND status='completed' ORDER BY id DESC LIMIT 1`, strings.ReplaceAll(p, "\\", "/")).Scan(&id); err != nil {
				writeJSONError(w, http.StatusNotFound, "selected media not found")
				return
			}
			body.IDs = append(body.IDs, id)
		}
	}
	if len(body.IDs) == 0 || len(body.IDs) > 100 {
		writeJSONError(w, http.StatusBadRequest, "select 1 to 100 media items")
		return
	}
	if strings.TrimSpace(body.Destination) == "" {
		writeJSONError(w, http.StatusBadRequest, "destination is required")
		return
	}
	type mediaRow struct {
		id                                                 int64
		groupID, groupName, messageID, name, path, caption string
	}
	rows := make([]mediaRow, 0, len(body.IDs))
	seen := map[int64]bool{}
	for _, id := range body.IDs {
		if id <= 0 || seen[id] {
			writeJSONError(w, http.StatusBadRequest, "media IDs must be positive and unique")
			return
		}
		seen[id] = true
		var row mediaRow
		row.id = id
		var recordID int64
		err := a.db.Reader.QueryRowContext(r.Context(), `SELECT id,group_id,group_name,message_id,file_name,file_path FROM downloads WHERE id=? AND status='completed'`, id).Scan(&recordID, &row.groupID, &row.groupName, &row.messageID, &row.name, &row.path)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "download not found")
			return
		}
		if row.path == "" {
			writeJSONError(w, http.StatusConflict, "download has no local media file")
			return
		}
		rows = append(rows, row)
	}
	for _, row := range rows {
		mode := "source_info"
		if body.Caption != "" {
			mode = "override"
		}
		route := map[string]any{"destinationGroupId": strings.TrimSpace(body.Destination), "captionMode": mode, "caption": body.Caption}
		settings := map[string]any{"enabled": true, "routes": []any{route}, "captionMode": mode, "caption": body.Caption}
		var msgID int
		_, _ = fmt.Sscan(row.messageID, &msgID)
		var histID int64
		_ = a.db.Writer.QueryRowContext(r.Context(), `SELECT id FROM tgdl_processing_attempts WHERE group_id=? AND message_id=? ORDER BY id DESC LIMIT 1`, row.groupID, msgID).Scan(&histID)
		a.queueForward(forwardJob{groupID: row.groupID, groupName: row.groupName, messageID: msgID, fileName: row.name, recordID: row.id, path: row.path, historyID: histID, settings: settings, stage: "available"})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "queued": len(body.IDs)})
}

func (a *App) handleGlobalForwardProtection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Protect bool `json:"protect"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	cfg, err := a.config.Load(r.Context())
	if err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	block, _ := cfg["forwarding"].(map[string]any)
	if block == nil {
		block = map[string]any{}
	}
	block["protect"] = body.Protect
	cfg["forwarding"] = block
	if err := a.config.Save(r.Context(), cfg); err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}

func (a *App) handleForwardQueueRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid forward job id")
		return
	}
	res, err := a.db.Writer.ExecContext(r.Context(), `UPDATE tgdl_forward_queue SET status='pending',reason='',completed_at=0,updated_at=? WHERE id=? AND status IN ('failed','interrupted')`, time.Now().UnixMilli(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSONError(w, http.StatusConflict, "forward job is not retryable")
		return
	}
	a.notifyForwardQueue()
	select {
	case a.forwards <- struct{}{}:
	default:
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (a *App) finishForwardQueue(ctx context.Context, job forwardJob, status, reason string) {
	if job.queueID == 0 {
		return
	}
	now := time.Now().UnixMilli()
	destinations, _ := json.Marshal(job.destinations)
	_, err := a.db.Writer.ExecContext(ctx, `UPDATE tgdl_forward_queue SET status=?,reason=CASE WHEN ?='' THEN reason ELSE ? END,stage=?,destinations_json=?,attempts=?,updated_at=?,completed_at=? WHERE id=?`, status, reason, reason, job.stage, string(destinations), job.attempts, now, now, job.queueID)
	if err != nil && a.output != nil {
		fmt.Fprintf(a.output, "AutoForward: could not record queue status for %s: %v\n", job.fileName, err)
	}
	a.notifyForwardQueue()
}

func (a *App) forwardQueueSnapshot(ctx context.Context) (map[string]any, error) {
	var stats struct {
		Total, Pending, Active, Forwarded, Failed, Interrupted, NotApplicable int64
	}
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(status='pending'),0),COALESCE(SUM(status='processing'),0),
		COALESCE(SUM(status='forwarded'),0),COALESCE(SUM(status='failed'),0),COALESCE(SUM(status='interrupted'),0),COALESCE(SUM(status='not_applicable'),0)
		FROM tgdl_forward_queue`).Scan(&stats.Total, &stats.Pending, &stats.Active, &stats.Forwarded, &stats.Failed, &stats.Interrupted, &stats.NotApplicable); err != nil {
		return nil, err
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT id,group_id,group_name,message_id,file_name,topic_id,status,reason,created_at,forward_config_json,stage,destinations_json FROM tgdl_forward_queue
		WHERE status IN ('pending','processing') OR id IN (SELECT id FROM tgdl_forward_queue WHERE status NOT IN ('pending','processing') ORDER BY id DESC LIMIT 50)
		ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'processing' THEN 1 ELSE 2 END,id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]map[string]any, 0, 50)
	cfg, _ := a.config.Load(ctx)
	for rows.Next() {
		var queueID int64
		var groupID, groupName, fileName, status, reason, forwardConfig, stage, destinationsJSON string
		var messageID int64
		var topicID int
		var createdAt int64
		if err := rows.Scan(&queueID, &groupID, &groupName, &messageID, &fileName, &topicID, &status, &reason, &createdAt, &forwardConfig, &stage, &destinationsJSON); err != nil {
			return nil, err
		}
		var destinations []string
		settings, _, _ := forwardSettings(cfg, groupID)
		if forwardConfig != "" {
			var snapshot forwardConfigSnapshot
			if json.Unmarshal([]byte(forwardConfig), &snapshot) == nil && snapshot.Settings != nil {
				settings = snapshot.Settings
			}
		}
		for _, route := range matchingForwardRoutes(settings, topicID) {
			destination := strings.TrimSpace(toString(route["destinationGroupId"]))
			if destination == "" {
				destination = strings.TrimSpace(toString(route["destination"]))
			}
			if destination != "" {
				destinations = append(destinations, destination)
			}
		}
		var destinationStates []forwardDestination
		_ = json.Unmarshal([]byte(destinationsJSON), &destinationStates)
		items = append(items, map[string]any{"id": queueID, "groupId": groupID, "groupName": groupName, "messageId": messageID, "fileName": fileName, "topicId": topicID, "destinations": destinations, "destinationStates": destinationStates, "stage": stage, "status": status, "reason": reason, "createdAt": createdAt})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"total": stats.Total - stats.NotApplicable, "pending": stats.Pending, "active": stats.Active, "forwarded": stats.Forwarded, "failed": stats.Failed, "interrupted": stats.Interrupted, "notApplicable": stats.NotApplicable, "items": items}, nil
}

// forwardSettings returns the chat's autoForward rule and forward account, or
// ok=false when the chat has no enabled rule.
func forwardSettings(cfg map[string]any, groupID string) (settings map[string]any, accountID string, ok bool) {
	for _, group := range configuredGroupList(cfg) {
		if toString(group["id"]) != groupID {
			continue
		}
		stored, _ := group["autoForward"].(map[string]any)
		if stored == nil {
			return nil, toString(group["forwardAccount"]), false
		}
		settings = cloneMap(stored)
		if _, present := settings["directForward"]; !present {
			settings["directForward"] = false
		}
		return settings, toString(group["forwardAccount"]), settings["enabled"] == true
	}
	return nil, "", false
}

func autoForwardEnabled(cfg map[string]any, groupID string) bool {
	_, _, enabled := forwardSettings(cfg, groupID)
	return enabled
}

func forwardRoutingSignature(settings map[string]any) string {
	var routes []string
	if raw, ok := settings["routes"].([]any); ok {
		for _, item := range raw {
			route, ok := item.(map[string]any)
			if !ok {
				continue
			}
			routes = append(routes, fmt.Sprintf("%d\x00%s\x00%d", int(number(route["sourceTopicId"], 0)), toString(route["destinationGroupId"]), int(number(route["destinationTopicId"], 0))))
		}
		sort.Strings(routes)
	}
	if len(routes) == 0 {
		return strings.TrimSpace(toString(settings["destinationGroupId"])) + "\x00" + strings.TrimSpace(toString(settings["destination"]))
	}
	return strings.Join(routes, "\x01")
}

func hasForwardDestination(settings map[string]any) bool {
	if routes, ok := settings["routes"].([]any); ok {
		return len(routes) > 0
	}
	return strings.TrimSpace(toString(settings["destinationGroupId"])) != "" || strings.TrimSpace(toString(settings["destination"])) != ""
}

func normalizedAutoForward(existing, value any) (map[string]any, error) {
	current, _ := existing.(map[string]any)
	incoming, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("autoForward must be an object")
	}
	out := make(map[string]any, len(current)+len(incoming))
	for key, item := range current {
		out[key] = item
	}
	for key, item := range incoming {
		out[key] = item
	}
	if enabled, ok := out["enabled"].(bool); !ok {
		out["enabled"] = false
	} else {
		out["enabled"] = enabled
	}
	if enabled, ok := out["deleteAfterForward"].(bool); !ok {
		out["deleteAfterForward"] = false
	} else {
		out["deleteAfterForward"] = enabled
	}
	if enabled, ok := out["directForward"].(bool); !ok {
		out["directForward"] = false
	} else {
		out["directForward"] = enabled
	}
	if out["deleteAfterForward"] != true || protectionValue(out["protect"]) == 1 {
		out["directForward"] = false
	}
	rawRoutes, validRoutes := out["routes"].([]any)
	_, hasRoutes := out["routes"]
	if hasRoutes && !validRoutes {
		return nil, errors.New("autoForward routes must be an array")
	}
	if len(rawRoutes) > 100 {
		return nil, errors.New("autoForward supports at most 100 routes")
	}
	routes := make([]any, 0, len(rawRoutes))
	for _, raw := range rawRoutes {
		original, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("autoForward route must be an object")
		}
		route := make(map[string]any, len(original))
		for key, value := range original {
			route[key] = value
		}
		destination := strings.TrimSpace(toString(route["destinationGroupId"]))
		if destination == "" {
			continue
		}
		mode := toString(route["captionMode"])
		if mode == "" {
			mode = "inherit"
		}
		legacyMode := mode
		if mode == "override" || mode == "regex" {
			mode = "custom"
		}
		if mode == "source_info" && route["captionModeOverride"] != true {
			mode = "inherit"
		}
		if mode == "source_info" {
			mode = "custom"
			route["captionBase"] = "custom"
			if toString(route["caption"]) == "" {
				route["caption"] = "{{source_caption}}\n\n{{source_info}}"
			}
		}
		if legacyMode == "override" {
			route["captionBase"] = "custom"
		} else if legacyMode == "regex" {
			route["captionBase"] = "source"
		}
		if mode == "custom" {
			base := toString(route["captionBase"])
			if base == "" {
				if toString(route["caption"]) != "" {
					base = "custom"
				} else {
					base = "source"
				}
			}
			if base != "source" && base != "custom" {
				return nil, errors.New("autoForward route has an invalid caption base")
			}
			route["captionBase"] = base
		}
		switch mode {
		case "inherit", "source_info", "source", "none", "custom", "override", "regex":
		default:
			return nil, errors.New("autoForward route has an invalid caption mode")
		}
		for _, key := range []string{"caption", "regexPattern", "regexReplacement"} {
			if len(toString(route[key])) > 4096 {
				return nil, errors.New("autoForward caption fields must be 4096 bytes or fewer")
			}
		}
		if (mode == "custom" || mode == "regex") && strings.TrimSpace(toString(route["regexPattern"])) != "" {
			pattern := toString(route["regexPattern"])
			if _, err := regexp.Compile(pattern); err != nil {
				return nil, fmt.Errorf("invalid caption regex: %w", err)
			}
		}
		route["destinationGroupId"] = destination
		route["captionMode"] = mode
		route["captionModeOverride"] = mode != "inherit"
		for _, key := range []string{"sourceTopicId", "destinationTopicId"} {
			n := float64(0)
			if value, present := route[key]; present {
				n = number(value, math.NaN())
			}
			if math.IsNaN(n) || n < 0 || n > 2147483647 || n != float64(int(n)) {
				return nil, errors.New("autoForward topic IDs must be non-negative integers")
			}
			route[key] = n
		}
		routes = append(routes, route)
	}
	seenRoutes := make(map[string]struct{}, len(routes))
	for _, raw := range routes {
		route := raw.(map[string]any)
		key := fmt.Sprintf("%d\x00%s", int(number(route["sourceTopicId"], 0)), toString(route["destinationGroupId"]))
		if _, exists := seenRoutes[key]; exists {
			return nil, errors.New("autoForward route duplicates a source topic and destination")
		}
		seenRoutes[key] = struct{}{}
	}
	mode := toString(out["captionMode"])
	if mode == "" {
		mode = "source"
	}
	legacyMode := mode
	if mode == "source_info" {
		mode = "custom"
		out["captionBase"] = "custom"
		if toString(out["caption"]) == "" {
			out["caption"] = "{{source_caption}}\n\n{{source_info}}"
		}
	}
	if mode == "override" || mode == "regex" {
		mode = "custom"
	}
	if legacyMode == "override" {
		out["captionBase"] = "custom"
	} else if legacyMode == "regex" {
		out["captionBase"] = "source"
	}
	switch mode {
	case "source_info", "source", "none", "custom", "override", "regex":
	default:
		return nil, errors.New("autoForward has an invalid caption mode")
	}
	if mode == "custom" {
		base := toString(out["captionBase"])
		if base == "" {
			if toString(out["caption"]) != "" {
				base = "custom"
			} else {
				base = "source"
			}
		}
		if base != "source" && base != "custom" {
			return nil, errors.New("autoForward has an invalid caption base")
		}
		out["captionBase"] = base
	}
	for _, key := range []string{"caption", "regexPattern", "regexReplacement"} {
		if len(toString(out[key])) > 4096 {
			return nil, errors.New("autoForward caption fields must be 4096 bytes or fewer")
		}
	}
	if (mode == "custom" || mode == "regex") && strings.TrimSpace(toString(out["regexPattern"])) != "" {
		pattern := toString(out["regexPattern"])
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("invalid caption regex: %w", err)
		}
	}
	if _, ok := out["protect"]; ok {
		if _, valid := out["protect"].(bool); !valid {
			if toString(out["protect"]) != "on" && toString(out["protect"]) != "off" {
				return nil, errors.New("autoForward protect must be on or off")
			}
		}
	}
	for i, value := range routes {
		route, _ := value.(map[string]any)
		if route == nil {
			continue
		}
		mode := toString(route["captionMode"])
		if mode == "inherit" {
			mode = toString(out["captionMode"])
		}
		if mode == "" {
			mode = "source_info"
		}
		route["captionMode"] = mode
		routes[i] = route
	}
	out["captionMode"] = mode
	if hasRoutes {
		out["routes"] = routes
	}
	return out, nil
}

func (a *App) forward(ctx context.Context, job forwardJob) {
	finishActivity := a.beginForward(job.groupID)
	defer finishActivity()
	status := "failed"
	reason := ""
	job.attempts++
	defer func() {
		if ctx.Err() != nil {
			status = "interrupted"
			reason = ctx.Err().Error()
		}
		a.finishForwardQueue(context.WithoutCancel(ctx), job, status, reason)
		a.finishProcessingForward(context.WithoutCancel(ctx), job.historyID, status)
		if status == "failed" && job.queueID > 0 && job.attempts < 3 && ctx.Err() == nil {
			delay := time.Second * time.Duration(1<<max(0, job.attempts-1))
			time.AfterFunc(delay, func() {
				res, err := a.db.Writer.ExecContext(context.WithoutCancel(ctx), `UPDATE tgdl_forward_queue SET status='pending',reason='',updated_at=? WHERE id=? AND status='failed' AND attempts=?`, time.Now().UnixMilli(), job.queueID, job.attempts)
				if err == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						select {
						case a.forwards <- struct{}{}:
						default:
						}
						a.notifyForwardQueue()
					}
				}
			})
		}
	}()
	if job.historyID != 0 {
		_, _ = a.db.Writer.ExecContext(context.WithoutCancel(ctx), `UPDATE tgdl_processing_attempts SET forward_status='processing',local_delete_status=CASE WHEN local_delete_status='pending' THEN 'pending' ELSE 'not_requested' END WHERE id=?`, job.historyID)
		a.notifyProcessingHistory()
	}
	globalConfig, err := a.config.Load(ctx)
	if err != nil {
		a.forwardFailed(job, "", err)
		return
	}
	// Queued automatic jobs carry a settings snapshot. The live toggle still
	// gates them; explicit manual forwards use stage "available".
	if job.stage != "available" {
		_, _, enabled := forwardSettings(globalConfig, job.groupID)
		if !enabled {
			status = "not_applicable"
			return
		}
	}
	groupSettings, accountID := job.settings, job.accountID
	if groupSettings == nil {
		groupSettings, accountID, _ = forwardSettings(globalConfig, job.groupID)
	}
	enabled := groupSettings["enabled"] == true
	if !enabled {
		status = "not_applicable"
		return
	}
	routes := matchingForwardRoutes(groupSettings, job.topicID)
	if len(routes) == 0 {
		status = "not_applicable"
		return
	}
	// The source chat is paused everywhere else; stay consistent.
	if blocked, _, err := a.accessBlocked(ctx, job.groupID); err != nil || blocked {
		return
	}
	if len(job.destinations) != len(routes) {
		job.destinations = make([]forwardDestination, len(routes))
	}
	needsUpload := false
	needsSourceDownload := false
	directCaptionEnabled := groupSettings["directForward"] == true && groupSettings["deleteAfterForward"] == true
	for i, route := range routes {
		settings := inheritCaptionSettings(route, groupSettings)
		settings = effectiveForwardProtection(settings, groupSettings, route, job.topicID, globalConfig)
		if job.destinations[i].Settings == nil {
			job.destinations[i] = forwardDestination{Settings: settings, Status: "pending"}
		} else {
			settings = job.destinations[i].Settings
		}
		job.destinations[i].Settings = settings
		if settings["protect"] == true {
			needsUpload = true
			needsSourceDownload = job.path == ""
		} else if customCaption(settings) && !directCaptionEnabled {
			needsUpload = true
			needsSourceDownload = job.path == ""
		}
	}
	if job.stage == "caption_edit_failed" {
		for i, route := range routes {
			if job.destinations[i].Status != "forwarded" && customCaption(job.destinations[i].Settings) && job.destinations[i].DestinationMessageID == 0 {
				a.forwardFailed(job, toString(route["destinationGroupId"]), errors.New("cannot safely retry caption edit: destination message id was not captured"))
				return
			}
		}
	}
	if needsSourceDownload {
		a.forwardFailed(job, "", errors.New("this forwarding route requires a completed download"))
		return
	}
	if needsUpload {
		job.stage = "upload"
	} else if job.path == "" {
		job.stage = "native_forward"
	} else {
		job.stage = "upload"
	}
	a.persistForwardProgress(ctx, job)
	for i, route := range routes {
		settings := inheritCaptionSettings(route, groupSettings)
		settings = effectiveForwardProtection(settings, groupSettings, route, job.topicID, globalConfig)
		if job.destinations[i].Settings != nil {
			settings = job.destinations[i].Settings
		}
		deleteAfter := job.recordID != 0 && groupSettings["deleteAfterForward"] == true && i == len(routes)-1
		if job.destinations[i].Status == "forwarded" {
			continue
		}
		job.destinations[i].Status = "processing"
		directCaption := job.path == "" && directCaptionEnabled && customCaption(settings) && settings["protect"] != true
		if directCaption && job.destinations[i].DestinationMessageID > 0 {
			job.destinations[i].Method = "caption_edit"
		} else if directCaption {
			job.destinations[i].Method = "native_forward_edit"
		} else if customCaption(settings) || settings["protect"] == true || job.path != "" {
			job.destinations[i].Method = "upload"
		} else {
			job.destinations[i].Method = "native_forward"
		}
		a.persistForwardProgress(ctx, job)
		if !a.forwardToRoute(ctx, &job, accountID, settings, deleteAfter) {
			job.destinations[i].Status = "failed"
			job.destinations[i].Reason = "delivery failed"
			return
		}
		job.destinations[i].Status, job.destinations[i].Reason = "forwarded", ""
		a.persistForwardProgress(ctx, job)
	}
	job.stage = "complete"
	status = "forwarded"
}

func (a *App) downloadForForward(ctx context.Context, job *forwardJob) error {
	if job.messageID <= 0 || a.monitor == nil {
		return errors.New("source message cannot be downloaded for protected forwarding")
	}
	return errors.New("source media needs a live Telegram session for download")
}

func customCaption(settings map[string]any) bool {
	switch toString(settings["captionMode"]) {
	case "native", "source":
		return false
	default:
		return true
	}
}

var captionVariablePattern = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)

func captionVariables(job forwardJob) map[string]string {
	sourceName := job.groupName
	if sourceName == "" {
		sourceName = job.groupID
	}
	link := ""
	if strings.HasPrefix(job.groupID, "-100") && job.messageID > 0 {
		channelID := strings.TrimPrefix(job.groupID, "-100")
		link = "https://t.me/c/" + channelID + "/" + strconv.Itoa(job.messageID)
	}
	return map[string]string{
		"source_caption": job.text,
		"source_name":    sourceName,
		"source_id":      job.groupID,
		"message_id":     strconv.Itoa(job.messageID),
		"source_info":    "📌 Source: " + sourceName,
		"source_link":    link,
	}
}

func expandCaptionVariables(value string, job forwardJob) string {
	variables := captionVariables(job)
	return captionVariablePattern.ReplaceAllStringFunc(value, func(token string) string {
		key := strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}")
		if replacement, ok := variables[key]; ok {
			return replacement
		}
		return token
	})
}

func effectiveForwardProtection(settings, group map[string]any, route map[string]any, topicID int, config ...map[string]any) map[string]any {
	out := make(map[string]any, len(settings)+1)
	for k, v := range settings {
		out[k] = v
	}
	delete(out, "protect")
	value := any(nil)
	if len(config) > 0 {
		block, _ := config[0]["forwarding"].(map[string]any)
		value = block["protect"]
	}
	if v, ok := group["protect"]; ok {
		value = v
	}
	out["protect"] = protectionValue(value) == 1
	return out
}

func protectionValue(v any) int {
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		if strings.EqualFold(x, "on") {
			return 1
		}
		if strings.EqualFold(x, "off") {
			return 0
		}
	}
	return -1
}

func (a *App) persistForwardProgress(ctx context.Context, job forwardJob) {
	if job.queueID == 0 {
		return
	}
	raw, _ := json.Marshal(job.destinations)
	_, _ = a.db.Writer.ExecContext(ctx, `UPDATE tgdl_forward_queue SET stage=?,destinations_json=?,updated_at=? WHERE id=?`, job.stage, string(raw), time.Now().UnixMilli(), job.queueID)
	a.notifyForwardQueue()
}

func forwardDestinationIndex(destinations []forwardDestination, destination string, topicID int) int {
	for i, state := range destinations {
		routeDestination := toString(state.Settings["destinationGroupId"])
		if routeDestination == "" {
			routeDestination = toString(state.Settings["destination"])
		}
		if routeDestination == destination && int(number(state.Settings["destinationTopicId"], 0)) == topicID {
			return i
		}
	}
	return -1
}

func (a *App) forwardToRoute(ctx context.Context, job *forwardJob, accountID string, settings map[string]any, deleteAfter bool) bool {
	destination := strings.TrimSpace(toString(settings["destinationGroupId"]))
	if destination == "" {
		destination = strings.TrimSpace(toString(settings["destination"]))
	}
	key := destinationKey(destination)
	destWasBlocked := false
	if key != "" {
		blocked, due, err := a.accessBlocked(ctx, key)
		if err != nil {
			a.forwardFailed(*job, destination, err)
			return false
		}
		if blocked && !due {
			// That forward is the re-check once its time comes.
			return false
		}
		destWasBlocked = blocked
	}
	directCaption := job.path == "" && job.settings["directForward"] == true && job.settings["deleteAfterForward"] == true && customCaption(settings) && settings["protect"] != true
	if (job.path == "" || job.stage == "native_forward") && (!customCaption(settings) || directCaption) && settings["protect"] != true && job.messageID > 0 {
		sender, ok := a.forwarder.(nativeForwardSender)
		var directErr error
		var destinationMessageID int
		var sentID int
		request := telegram.ForwardMessageRequest{Source: job.groupID, Destination: destination, MessageID: job.messageID, TopicID: int(number(settings["destinationTopicId"], 0)), Protect: false}
		if !ok {
			directErr = errors.New("native Telegram forwarding is unavailable")
		} else {
			if directCaption {
				caption, entities, captionErr := routeCaption(settings, *job)
				if captionErr != nil {
					return false
				}
				request.EditCaption, request.Caption, request.Entities = true, caption, entities
				request.ResultMessageID = &destinationMessageID
				if i := forwardDestinationIndex(job.destinations, destination, request.TopicID); i >= 0 {
					request.EditMessageID = job.destinations[i].DestinationMessageID
				}
			}
			_, sentID, directErr = sender.ForwardMessage(ctx, accountID, request)
			if destinationMessageID == 0 {
				destinationMessageID = sentID
			}
		}
		if i := forwardDestinationIndex(job.destinations, destination, request.TopicID); i >= 0 {
			job.destinations[i].DestinationMessageID = destinationMessageID
			if directErr != nil {
				job.destinations[i].DirectFailed = true
				job.destinations[i].Reason = "direct forward failed"
				job.destinations[i].Method = "upload"
			} else {
				job.destinations[i].DirectFailed = false
				job.destinations[i].Reason = ""
				if directCaption {
					job.destinations[i].Method = "caption_edit"
				}
			}
		}
		if directErr == nil {
			return true
		}
		a.persistForwardProgress(ctx, *job)
		// The source may disallow native forwarding while still allowing the
		// application account to download and send an accessible media file.
		if job.path == "" {
			a.forwardFailed(*job, destination, directErr)
			return false
		}
		job.stage = "fallback_upload"
		a.persistForwardProgress(ctx, *job)
	}
	file, err := openMedia(a.downloadsDir, job.path)
	if err != nil {
		a.forwardFailed(*job, destination, fmt.Errorf("open downloaded file: %w", err))
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		a.forwardFailed(*job, destination, err)
		return false
	}
	caption, entities, err := routeCaption(settings, *job)
	if err != nil {
		a.forwardFailed(*job, destination, err)
		return false
	}
	req := telegram.SendFileRequest{Destination: destination, TopicID: int(number(settings["destinationTopicId"], 0)), Name: path.Base(job.path), Size: info.Size(), Facts: job.facts, Caption: caption, Entities: entities, Protect: settings["protect"] == true}
	var used string
	var sent int
	floodWaitAttempts := a.forwarderConfig(ctx).floodWaitAttempts
	for attempt := 1; ; attempt++ {
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			break
		}
		req.Body = file
		attemptCtx, cancel := context.WithTimeout(ctx, forwardTimeout(info.Size()))
		used, sent, err = a.forwarder.SendFile(attemptCtx, accountID, req)
		cancel()
		wait, flood := tgerr.AsFloodWait(err)
		if !flood || attempt >= floodWaitAttempts {
			break
		}
		// Telegram asked this account to wait; the flood guard keeps every
		// other request off it meanwhile. Try again once the wait is over.
		timer := time.NewTimer(wait + time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		a.forwardFailed(*job, destination, err)
		if key != "" {
			if state, code, definite := classifyAccessError(err); definite {
				if recordErr := a.recordDestinationAccess(ctx, key, used, state, code, destWasBlocked); recordErr != nil && a.output != nil {
					fmt.Fprintf(a.output, "AutoForward: could not record destination access: %v\n", recordErr)
				}
			}
		}
		return false
	}
	if key != "" {
		if err := a.markDestinationReachable(ctx, key, used); err != nil && a.output != nil {
			fmt.Fprintf(a.output, "AutoForward: could not record destination access: %v\n", err)
		}
	}
	if a.output != nil {
		label := destination
		if label == "" || label == "storage" {
			label = telegram.StorageChannelTitle
		}
		tail := ""
		if sent > 0 {
			tail = fmt.Sprintf(" (msg #%d)", sent)
		}
		fmt.Fprintf(a.output, "AutoForward: sent %s from %s to %s%s\n", req.Name, job.groupName, label, tail)
	}
	if deleteAfter {
		file.Close()
		a.deleteForwarded(ctx, *job)
	}
	return true
}

func inheritCaptionSettings(route, group map[string]any) map[string]any {
	if _, routeSpecific := route["destinationGroupId"]; !routeSpecific || toString(route["captionMode"]) != "inherit" {
		return route
	}
	out := make(map[string]any, len(route)+4)
	for key, value := range route {
		out[key] = value
	}
	for _, key := range []string{"captionMode", "captionBase", "caption", "regexPattern", "regexReplacement"} {
		if value, ok := group[key]; ok {
			out[key] = value
		}
	}
	return out
}

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func matchingForwardRoutes(settings map[string]any, sourceTopicID int) []map[string]any {
	raw, present := settings["routes"]
	if !present {
		return []map[string]any{settings}
	}
	routes, ok := raw.([]any)
	if !ok {
		return nil
	}
	var exact, fallback []map[string]any
	for _, raw := range routes {
		route, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		topicID := int(number(route["sourceTopicId"], 0))
		if topicID == sourceTopicID && sourceTopicID != 0 {
			exact = append(exact, route)
		}
		if topicID == 0 {
			fallback = append(fallback, route)
		}
	}
	destinations := make(map[string]bool)
	for _, route := range exact {
		destinations[toString(route["destinationGroupId"])] = true
	}
	for _, route := range fallback {
		if !destinations[toString(route["destinationGroupId"])] {
			exact = append(exact, route)
		}
	}
	return exact
}

func routeCaption(settings map[string]any, job forwardJob) (string, []tg.MessageEntityClass, error) {
	switch toString(settings["captionMode"]) {
	case "none":
		return "", nil, nil
	case "source":
		return telegram.PlainForwardCaption(job.text), nil, nil
	case "override":
		return telegram.PlainForwardCaption(expandCaptionVariables(toString(settings["caption"]), job)), nil, nil
	case "regex":
		pattern, err := regexp.Compile(toString(settings["regexPattern"]))
		if err != nil {
			return "", nil, fmt.Errorf("invalid caption regex: %w", err)
		}
		return telegram.PlainForwardCaption(pattern.ReplaceAllString(job.text, expandCaptionVariables(toString(settings["regexReplacement"]), job))), nil, nil
	case "custom":
		caption := job.text
		if toString(settings["captionBase"]) == "custom" || (toString(settings["captionBase"]) == "" && toString(settings["caption"]) != "") {
			caption = toString(settings["caption"])
		}
		caption = expandCaptionVariables(caption, job)
		patternText := strings.TrimSpace(toString(settings["regexPattern"]))
		if patternText != "" {
			pattern, err := regexp.Compile(patternText)
			if err != nil {
				return "", nil, fmt.Errorf("invalid caption regex: %w", err)
			}
			caption = pattern.ReplaceAllString(caption, expandCaptionVariables(toString(settings["regexReplacement"]), job))
		}
		return telegram.PlainForwardCaption(caption), nil, nil
	default:
		caption, entities := telegram.ForwardCaption(job.text, job.groupName, job.groupID, job.messageID)
		return caption, entities, nil
	}
}

// deleteForwarded removes the local copy, as 2.x's unlink did. The catalog
// row goes with it (2.x's integrity sweep dropped it later), and the file
// itself stays when another row shares it (a deduplicated download).
func (a *App) deleteForwarded(ctx context.Context, job forwardJob) {
	deleted, err := a.deleteByWhere(contextRequest(ctx), `id=? AND REPLACE(file_path,char(92),'/')=?`, job.recordID, job.path)
	if err != nil {
		a.setProcessingDeleteStatus(context.WithoutCancel(ctx), job.historyID, "failed", err.Error())
		if a.output != nil {
			fmt.Fprintf(a.output, "AutoForward: forwarded but local delete failed for %s: %v\n", path.Base(job.path), err)
		}
		return
	}
	if deleted == 0 {
		a.setProcessingDeleteStatus(context.WithoutCancel(ctx), job.historyID, "already_absent", "local download record was already removed")
		return
	}
	a.markProcessingDeleted(ctx, job.historyID)
	payload := map[string]any{"path": job.path}
	if remaining, err := openMedia(a.downloadsDir, job.path); err == nil {
		remaining.Close()
		payload = map[string]any{"id": job.recordID}
	}
	a.hub.Broadcast(ws.Event{Type: "file_deleted", Flat: true, Payload: payload})
	if a.output != nil {
		fmt.Fprintf(a.output, "AutoForward: deleted local file %s\n", path.Base(job.path))
	}
}

func (a *App) forwardFailed(job forwardJob, destination string, err error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a.failProcessingForward(context.WithoutCancel(ctx), job.historyID, err)
	a.finishForwardQueue(context.WithoutCancel(ctx), job, "failed", err.Error())
	if a.output != nil {
		fmt.Fprintf(a.output, "AutoForward: %s message %d failed: %v\n", job.groupName, job.messageID, err)
	}
	a.hub.Broadcast(ws.Event{Type: "forward_error", Payload: map[string]any{
		"groupId": job.groupID, "groupName": job.groupName, "messageId": job.messageID, "destination": destination, "error": err.Error(),
	}})
}

// Bound one upload like a download attempt: 2 minutes plus 1 s per 16 KiB.
func forwardTimeout(size int64) time.Duration {
	return min(2*time.Minute+time.Duration(max(size, 0)/(16*1024))*time.Second, 24*time.Hour)
}

// ---- Destination access (2.x chat-access rows under a "dest:" key) ---------

var numericID = regexp.MustCompile(`^-?\d+$`)

// destinationKey is 2.x's registry key for a forward destination:
// "dest:<chat id>" or "dest:@username". It is namespaced so a destination
// that refuses posts never pauses reading the same chat. Saved Messages and
// the storage channel have none.
func destinationKey(destination string) string {
	d := strings.TrimSpace(destination)
	switch d {
	case "", "storage", "me", "saved":
		return ""
	}
	if numericID.MatchString(d) {
		return "dest:" + d
	}
	return "dest:@" + strings.ToLower(strings.TrimPrefix(d, "@"))
}

// Telegram error names that say something definite about a chat (2.x's
// CODE_STATES).
var accessErrorStates = map[string]string{
	"CHANNEL_PRIVATE": "private", "CHANNEL_INVALID": "deleted", "CHAT_ID_INVALID": "deleted",
	"PEER_ID_INVALID": "deleted", "USERNAME_INVALID": "deleted", "USERNAME_NOT_OCCUPIED": "deleted",
	"CHAT_FORBIDDEN": "banned", "USER_BANNED_IN_CHANNEL": "banned", "USER_KICKED": "banned",
	"USER_NOT_PARTICIPANT": "left", "CHANNEL_PUBLIC_GROUP_NA": "restricted", "CHAT_RESTRICTED": "restricted",
	"CHAT_WRITE_FORBIDDEN": "restricted", "CHAT_SEND_MEDIA_FORBIDDEN": "restricted", "CHAT_ADMIN_REQUIRED": "restricted",
}

var accessPriority = map[string]int{"migrated": 7, "deleted": 6, "banned": 5, "restricted": 4, "private": 3, "left": 2}

func classifyAccessError(err error) (state, code string, definite bool) {
	if rpc, ok := tgerr.As(err); ok {
		if state, found := accessErrorStates[rpc.Type]; found {
			return state, rpc.Type, true
		}
	}
	if errors.Is(err, telegram.ErrDestinationNotFound) {
		return "left", "ENTITY_NOT_FOUND", true
	}
	return "", "", false
}

// accessBlocked reads a chat_access row: blocked, and whether its scheduled
// re-check time has come.
func (a *App) accessBlocked(ctx context.Context, id string) (blocked, due bool, err error) {
	var state string
	var next sql.NullInt64
	err = a.db.Reader.QueryRowContext(ctx, `SELECT state,next_check_at FROM chat_access WHERE chat_id=?`, id).Scan(&state, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	blocked = blockingChatState(state)
	return blocked, blocked && next.Valid && next.Int64 <= time.Now().UnixMilli(), nil
}

type accountAccess struct {
	State  string `json:"state"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
	At     int64  `json:"at"`
}

// Re-check delays after the 1st, 2nd and 3rd+ failed check (±10 %).
var recheckSchedule = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}

func recheckDelay(checks int) time.Duration {
	base := recheckSchedule[min(checks, len(recheckSchedule)-1)]
	return time.Duration(float64(base) * (0.9 + 0.2*rand.Float64()))
}

// recordDestinationAccess is 2.x's recordResult for one account's definite
// refusal: the destination is paused until its re-check time.
func (a *App) recordDestinationAccess(ctx context.Context, key, accountID, state, code string, isRecheck bool) error {
	return a.updateDestinationAccess(ctx, key, accountID, accountAccess{State: state, Code: code}, isRecheck)
}

// markDestinationReachable is 2.x's markReachable: a successful post clears
// the row once no account is failing.
func (a *App) markDestinationReachable(ctx context.Context, key, accountID string) error {
	return a.updateDestinationAccess(ctx, key, accountID, accountAccess{State: "ok"}, false)
}

func (a *App) updateDestinationAccess(ctx context.Context, key, accountID string, result accountAccess, isRecheck bool) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prevState string
	var accountsJSON sql.NullString
	var firstSeen, next sql.NullInt64
	var checks int
	err = tx.QueryRowContext(ctx, `SELECT state,accounts,first_seen_at,next_check_at,checks FROM chat_access WHERE chat_id=?`, key).Scan(&prevState, &accountsJSON, &firstSeen, &next, &checks)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !exists && result.State == "ok" {
		return nil
	}
	accounts := map[string]accountAccess{}
	if accountsJSON.Valid {
		_ = json.Unmarshal([]byte(accountsJSON.String), &accounts)
	}
	if accountID == "" {
		accountID = "_"
	}
	now := time.Now().UnixMilli()
	result.At = now
	accounts[accountID] = result
	var verdict *accountAccess
	anyOK, failing := false, false
	for id := range accounts {
		entry := accounts[id]
		switch {
		case entry.State == "ok":
			anyOK = true
		case accessPriority[entry.State] > 0:
			failing = true
			if verdict == nil || accessPriority[entry.State] > accessPriority[verdict.State] {
				verdict = &entry
			}
		}
	}
	if anyOK && !failing {
		if _, err = tx.ExecContext(ctx, `DELETE FROM chat_access WHERE chat_id=?`, key); err != nil {
			return err
		}
		return tx.Commit()
	}
	encoded, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	if anyOK || verdict == nil {
		// Postable through another account: keep the per-account note only.
		_, err = tx.ExecContext(ctx, `INSERT INTO chat_access(chat_id,state,code,detail,migrated_to,first_seen_at,checked_at,next_check_at,checks,accounts,updated_at)
 VALUES(?,'ok',NULL,NULL,NULL,NULL,?,NULL,0,?,?) ON CONFLICT(chat_id) DO UPDATE SET state='ok',code=NULL,detail=NULL,migrated_to=NULL,first_seen_at=NULL,
 checked_at=excluded.checked_at,next_check_at=NULL,checks=0,accounts=excluded.accounts,updated_at=excluded.updated_at`, key, now, string(encoded), now)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	wasBlocked := exists && accessPriority[prevState] > 0
	nextCheck := now + recheckDelay(0).Milliseconds()
	if wasBlocked {
		if isRecheck {
			checks++
			nextCheck = now + recheckDelay(checks).Milliseconds()
		} else if next.Valid {
			nextCheck = next.Int64
		} else {
			nextCheck = now + recheckDelay(checks).Milliseconds()
		}
	} else {
		checks = 0
	}
	first := now
	if wasBlocked && firstSeen.Valid {
		first = firstSeen.Int64
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO chat_access(chat_id,state,code,detail,migrated_to,first_seen_at,checked_at,next_check_at,checks,accounts,updated_at)
 VALUES(?,?,?,NULL,NULL,?,?,?,?,?,?) ON CONFLICT(chat_id) DO UPDATE SET state=excluded.state,code=excluded.code,
 detail=CASE WHEN chat_access.state=excluded.state THEN chat_access.detail ELSE NULL END,migrated_to=NULL,first_seen_at=excluded.first_seen_at,
 checked_at=excluded.checked_at,next_check_at=excluded.next_check_at,checks=excluded.checks,accounts=excluded.accounts,updated_at=excluded.updated_at`,
		key, verdict.State, nullIfEmpty(verdict.Code), first, now, nextCheck, checks, string(encoded), now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// forwardCandidate decides, before ingestion, whether this download would be
// auto-forwarded: 2.x forwarded each newly downloaded message while its
// runtime (the live monitor) was running, and never a message it already had.
func (a *App) forwardCandidate(ctx context.Context, groupID string, messageID int64) (bool, map[string]any, string, error) {
	if a.forwarder == nil || !a.forwarder.Observing() {
		return false, nil, "", nil
	}
	a.forwardActivityMu.Lock()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		a.forwardActivityMu.Unlock()
		return false, nil, "", err
	}
	settings, accountID, enabled := forwardSettings(cfg, groupID)
	a.forwardActivityMu.Unlock()
	if !enabled {
		return false, nil, "", nil
	}
	var exists bool
	err = a.db.Reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM downloads WHERE group_id=? AND message_id=?)`, groupID, messageID).Scan(&exists)
	return !exists, settings, accountID, err
}
