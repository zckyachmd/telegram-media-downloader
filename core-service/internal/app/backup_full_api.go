package app

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/backup"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

const maxFullBackupUpload = int64(65 << 30)

func registerFullBackupRoutes(mux *http.ServeMux, a *App) {
	mux.Handle("GET /api/backup/full/status", a.requireAdmin(http.HandlerFunc(a.handleFullBackupStatus)))
	mux.Handle("GET /api/backup/full/latest", a.requireAdmin(http.HandlerFunc(a.handleFullBackupLatest)))
	mux.Handle("GET /api/backup/full/download", a.requireAdmin(http.HandlerFunc(a.handleFullBackupDownload)))
	mux.Handle("POST /api/backup/full/run", a.requireAdmin(http.HandlerFunc(a.handleFullBackupRun)))
	mux.Handle("POST /api/backup/full/restore-upload", a.requireAdmin(http.HandlerFunc(a.handleFullRestoreUpload)))
}

func (a *App) handleFullBackupStatus(w http.ResponseWriter, r *http.Request) {
	objects, err := a.backups.FullSnapshots(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.fullOperationMu.Lock()
	var status map[string]any
	if a.fullOperationStatus == nil {
		status = map[string]any{"running": false, "stage": "idle"}
	} else {
		status = cloneConfigValue(a.fullOperationStatus).(map[string]any)
	}
	a.fullOperationMu.Unlock()
	result := map[string]any{"success": true, "operation": status}
	if len(objects) > 0 {
		latest := objects[0]
		result["latest"] = map[string]any{"path": latest.Path, "size": latest.Size, "modifiedAt": latest.Modified.UnixMilli()}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *App) handleFullBackupLatest(w http.ResponseWriter, r *http.Request) {
	objects, err := a.backups.FullSnapshots(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(objects) == 0 {
		writeJSONError(w, http.StatusNotFound, "no local full backup is available")
		return
	}
	latest := objects[0]
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "backup": map[string]any{"path": latest.Path, "size": latest.Size, "modifiedAt": latest.Modified.UnixMilli()}})
}

func (a *App) handleFullBackupDownload(w http.ResponseWriter, r *http.Request) {
	objects, err := a.backups.FullSnapshots(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(objects) == 0 {
		writeJSONError(w, http.StatusNotFound, "no local full backup is available")
		return
	}
	path, err := a.backups.FullSnapshotPath(objects[0].Path)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(objects[0].Path)+`"`)
	http.ServeFile(w, r, path)
}

func (a *App) handleFullBackupRun(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if decodeBody(w, r, &input) != nil {
		return
	}
	if len(input.Password) < 12 || len(input.Password) > 1024 {
		writeJSONError(w, http.StatusBadRequest, "backup password must be between 12 and 1024 characters")
		return
	}
	if !a.beginFullOperation("backup", "queued") {
		writeJSONError(w, http.StatusConflict, "a full backup or restore is already running")
		return
	}
	if !a.launchMaintenance(func() { a.runFullBackup(input.Password) }) {
		a.setFullOperation(false, "error", "server is stopping")
		writeJSONError(w, http.StatusServiceUnavailable, "server is stopping")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "started": true})
}

func (a *App) runFullBackup(password string) {
	defer clear([]byte(password))
	a.setFullOperation(true, "creating", "")
	_, err := a.backups.CreateFullSnapshotWithPassword(a.ctx, password)
	if err != nil {
		a.setFullOperation(false, "error", err.Error())
		a.backups.Log("error", "full backup failed: "+err.Error())
		a.hub.Broadcast(ws.Event{Type: "full_backup_error", Flat: true, Payload: map[string]any{"error": err.Error()}, Roles: []string{"admin"}})
		return
	}
	a.setFullOperation(false, "done", "")
	a.hub.Broadcast(ws.Event{Type: "full_backup_done", Flat: true, Payload: map[string]any{"stage": "done"}, Roles: []string{"admin"}})
}

func (a *App) handleFullRestoreUpload(w http.ResponseWriter, r *http.Request) {
	if a.restart == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "server restart is unavailable")
		return
	}
	if !a.beginFullOperation("restore", "uploading") {
		writeJSONError(w, http.StatusConflict, "a full backup or restore is already running")
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			a.setFullOperation(false, "error", "restore upload failed")
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxFullBackupUpload+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid restore upload or file exceeds 65 GiB")
		return
	}
	password := r.FormValue("password")
	if len(password) == 0 || len(password) > 1024 {
		writeJSONError(w, http.StatusBadRequest, "backup password is required")
		return
	}
	file, header, err := r.FormFile("backup")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "select a full backup file")
		return
	}
	defer file.Close()
	if header.Size < 54 || header.Size > maxFullBackupUpload {
		writeJSONError(w, http.StatusBadRequest, "backup file size is invalid or exceeds 65 GiB")
		return
	}
	folder := filepath.Join(a.dataDir, "backups")
	if err = os.MkdirAll(folder, 0700); err != nil {
		a.setFullOperation(false, "error", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	upload, err := os.CreateTemp(folder, ".full-upload-*.tgdf")
	if err != nil {
		a.setFullOperation(false, "error", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "could not stage uploaded backup")
		return
	}
	uploadPath := upload.Name()
	if err = upload.Chmod(0600); err == nil {
		var n int64
		n, err = io.Copy(upload, file)
		if err == nil && n != header.Size {
			err = fmt.Errorf("backup upload size mismatch")
		}
	}
	if closeErr := upload.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(uploadPath)
		a.setFullOperation(false, "error", err.Error())
		writeJSONError(w, http.StatusBadRequest, "could not stage uploaded backup")
		return
	}
	archive, err := a.backups.DecryptFullSnapshotWithPassword(r.Context(), uploadPath, password)
	_ = os.Remove(uploadPath)
	if err != nil {
		a.setFullOperation(false, "error", err.Error())
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.fullOperationMu.Lock()
	a.fullRestorePassword = []byte(password)
	a.fullOperationMu.Unlock()
	clear([]byte(password))
	if !a.launchMaintenance(func() { a.runFullRestore(archive) }) {
		_ = os.Remove(archive)
		a.fullOperationMu.Lock()
		clear(a.fullRestorePassword)
		a.fullRestorePassword = nil
		a.fullOperationMu.Unlock()
		a.setFullOperation(false, "error", "server is stopping")
		writeJSONError(w, http.StatusServiceUnavailable, "server is stopping")
		return
	}
	accepted = true
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "started": true})
}

func (a *App) runFullRestore(archive string) {
	ctx := a.ctx
	defer os.Remove(archive)
	defer func() {
		a.fullOperationMu.Lock()
		clear(a.fullRestorePassword)
		a.fullRestorePassword = nil
		a.fullOperationMu.Unlock()
	}()
	finish := func(err error) {
		if err != nil {
			a.setFullOperation(false, "error", err.Error())
			a.backups.Log("error", "full restore failed: "+err.Error())
			a.hub.Broadcast(ws.Event{Type: "full_restore_error", Flat: true, Payload: map[string]any{"error": err.Error()}, Roles: []string{"admin"}})
			return
		}
		a.setFullOperation(false, "restarting", "")
		a.hub.Broadcast(ws.Event{Type: "full_restore_ready", Flat: true, Payload: map[string]any{"stage": "restarting"}, Roles: []string{"admin"}})
	}
	a.setFullOperation(true, "creating_rollback", "")
	a.fullOperationMu.Lock()
	rollbackPassword := string(a.fullRestorePassword)
	a.fullOperationMu.Unlock()
	if rollbackPassword == "" {
		finish(fmt.Errorf("restore password unavailable for rollback"))
		return
	}
	if _, err := a.backups.CreateFullSnapshotWithPassword(ctx, rollbackPassword); err != nil {
		clear([]byte(rollbackPassword))
		finish(fmt.Errorf("create rollback backup: %w", err))
		return
	}
	clear([]byte(rollbackPassword))
	a.setFullOperation(true, "validating", "")
	parent := filepath.Dir(a.dataDir)
	stage, err := os.MkdirTemp(parent, ".tgdl-full-restore-")
	if err != nil {
		finish(err)
		return
	}
	if err = os.Remove(stage); err != nil {
		finish(err)
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()
	_, err = backup.RestoreSnapshot(ctx, archive, stage, backup.RestoreOptions{Plaintext: true, Full: true, MaxBytes: 1 << 50, MaxFiles: 1000000})
	if err != nil {
		finish(err)
		return
	}
	a.setFullOperation(true, "preparing_files", "")
	if err = backup.PrepareFullRestore(a.dataDir, a.downloadsDir, stage); err != nil {
		finish(err)
		return
	}
	keep = true
	finish(nil)
	go func() {
		select {
		case <-time.After(500 * time.Millisecond):
			a.restart()
		case <-ctx.Done():
		}
	}()
}

func (a *App) beginFullOperation(kind, stage string) bool {
	a.fullOperationMu.Lock()
	defer a.fullOperationMu.Unlock()
	if a.fullOperationRunning {
		return false
	}
	a.fullOperationRunning = true
	a.fullOperationStatus = map[string]any{"running": true, "type": kind, "stage": stage}
	return true
}

func (a *App) setFullOperation(running bool, stage, message string) {
	a.fullOperationMu.Lock()
	status := map[string]any{"running": running, "stage": stage}
	if current, ok := a.fullOperationStatus["type"]; ok {
		status["type"] = current
	}
	if message != "" {
		status["error"] = message
	}
	a.fullOperationStatus = status
	a.fullOperationRunning = running
	a.fullOperationMu.Unlock()
}
