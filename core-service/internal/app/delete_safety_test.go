package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPDeleteOnlySelectedReferenceAndItsCaches(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	files := []string{
		filepath.Join(a.dataDir, "downloads", "gallery", "photo.jpg"),
		thumbCachePath(filepath.Join(a.dataDir, "thumbs"), 1),
		thumbCachePath(filepath.Join(a.dataDir, "thumbs"), 2),
		filepath.Join(a.dataDir, "seekbar", "1.webp"),
		filepath.Join(a.dataDir, "seekbar", "1.json"),
	}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_path) VALUES('1',1,'gallery/photo.jpg'),('2',2,'gallery\photo.jpg')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO seekbar_sprites(download_id,sprite_path,meta_path,generated_at) VALUES(1,?,?,1)`, files[3], files[4]); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("DELETE", "/api/file?path="+url.QueryEscape("gallery/photo.jpg")+"&id=1", nil)
	w := httptest.NewRecorder()
	a.handleAPIFileDelete(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	for i, file := range files {
		_, err := os.Stat(file)
		if i == 0 || i == 2 {
			if err != nil {
				t.Errorf("remaining owner's file lost: %s: %v", file, err)
			}
		} else if !os.IsNotExist(err) {
			t.Errorf("deleted row's cache remains: %s: %v", file, err)
		}
	}
	var count int
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE id=2`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("other reference removed: count=%d err=%v", count, err)
	}
}

func TestBulkDeleteRejectsOverlappingDuplicateFinderJob(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.dedupMu.Lock()
	a.dedupDeleteStatus["running"] = true
	a.dedupMu.Unlock()
	for _, handler := range []http.HandlerFunc{a.handleDedupDelete, a.handleAPIBulkDelete} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"ids":[1]}`)))
		if w.Code != http.StatusConflict {
			t.Fatalf("overlapping delete: %d %s", w.Code, w.Body.String())
		}
	}
	a.dedupMu.Lock()
	a.dedupDeleteStatus["running"] = false
	a.dedupMu.Unlock()
}

func TestDeleteSharedFileKeepsRemainingOwner(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root := filepath.Join(a.dataDir, "downloads")
	if err := os.MkdirAll(filepath.Join(root, "gallery"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "gallery", "photo.jpg")
	if err := os.WriteFile(file, []byte("shared media"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_path) VALUES('1',1,'gallery/photo.jpg'),('2',2,'gallery\photo.jpg')`); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	if _, err := a.deleteRows(r, []int64{1}, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "shared media" {
		t.Fatalf("remaining owner's media lost: %q %v", got, err)
	}
	if _, err := a.deleteRows(r, []int64{2}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("last owner's media remains: %v", err)
	}
}

func TestEvictMirroredDownloadKeepsCatalogAndBackupRecord(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	file := filepath.Join(a.downloadsDir, "archive", "media.bin")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("mirrored media"), 0600); err != nil {
		t.Fatal(err)
	}
	dest, err := a.db.Writer.Exec(`INSERT INTO backup_destinations(name,provider,config_blob,mode,created_at) VALUES('Drive','gdrive',X'00','mirror',1)`)
	if err != nil {
		t.Fatal(err)
	}
	destinationID, _ := dest.LastInsertId()
	media, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_path,file_size,status) VALUES('g',7,'media.bin','archive/media.bin',14,'completed')`)
	if err != nil {
		t.Fatal(err)
	}
	downloadID, _ := media.LastInsertId()
	if _, err := a.db.Writer.Exec(`UPDATE backup_jobs SET status='done',remote_path='archive/media.bin',bytes_uploaded=14 WHERE destination_id=? AND download_id=?`, destinationID, downloadID); err != nil {
		t.Fatal(err)
	}
	removed, err := a.evictMirroredDownload(context.Background(), downloadID, "archive/media.bin")
	if err != nil || !removed {
		t.Fatalf("evict mirrored file: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("local file remains: %v", err)
	}
	var path, remote, status string
	var size, jobs int
	if err := a.db.Reader.QueryRow(`SELECT COALESCE(file_path,''),file_size FROM downloads WHERE id=?`, downloadID).Scan(&path, &size); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Reader.QueryRow(`SELECT remote_path,status FROM backup_jobs WHERE download_id=?`, downloadID).Scan(&remote, &status); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE group_id='g' AND message_id=7`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if path != "" || size != 14 || remote != "archive/media.bin" || status != "done" || jobs != 1 {
		t.Fatalf("catalog or mirror history lost: path=%q size=%d remote=%q status=%q rows=%d", path, size, remote, status, jobs)
	}
	files, total, err := a.queryGalleryFiles(httptest.NewRequest(http.MethodGet, "/api/downloads/all", nil), galleryQuery{limit: 50})
	if err != nil || total != 0 || len(files) != 0 {
		t.Fatalf("evicted media still appears in gallery: total=%d files=%d err=%v", total, len(files), err)
	}
}

func TestEvictMirroredDownloadWaitsForForwardAndKeepsItsHistory(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	file := filepath.Join(a.downloadsDir, "archive", "forward.bin")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	dest, err := a.db.Writer.Exec(`INSERT INTO backup_destinations(name,provider,config_blob,mode,created_at) VALUES('Drive','gdrive',X'00','mirror',1)`)
	if err != nil {
		t.Fatal(err)
	}
	destinationID, _ := dest.LastInsertId()
	media, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_path,file_size,status) VALUES('g',8,'forward.bin','archive/forward.bin',5,'completed')`)
	if err != nil {
		t.Fatal(err)
	}
	downloadID, _ := media.LastInsertId()
	if _, err := a.db.Writer.Exec(`UPDATE backup_jobs SET status='done',remote_path='archive/forward.bin' WHERE destination_id=? AND download_id=?`, destinationID, downloadID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO tgdl_forward_queue(group_id,message_id,record_id,file_path,status,created_at,updated_at) VALUES('g',8,?,'archive/forward.bin','processing',1,1)`, downloadID); err != nil {
		t.Fatal(err)
	}
	removed, err := a.evictMirroredDownload(context.Background(), downloadID, "archive/forward.bin")
	if err != nil || removed {
		t.Fatalf("active forward should defer mirror eviction: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("forward source was removed before completion: %v", err)
	}
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_forward_queue SET status='forwarded' WHERE group_id='g' AND message_id=8`); err != nil {
		t.Fatal(err)
	}
	removed, err = a.evictMirroredDownload(context.Background(), downloadID, "archive/forward.bin")
	if err != nil || !removed {
		t.Fatalf("completed forward should allow mirror eviction: removed=%v err=%v", removed, err)
	}
	var downloadRows, backupRows, forwardRows int
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE id=? AND file_path IS NULL`, downloadID).Scan(&downloadRows); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM backup_jobs WHERE download_id=? AND status='done'`, downloadID).Scan(&backupRows); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_forward_queue WHERE group_id='g' AND message_id=8 AND status='forwarded'`).Scan(&forwardRows); err != nil {
		t.Fatal(err)
	}
	if downloadRows != 1 || backupRows != 1 || forwardRows != 1 {
		t.Fatalf("history after cleanup: downloads=%d backups=%d forwards=%d", downloadRows, backupRows, forwardRows)
	}
}

func TestDeleteBatchBeyondSQLiteExpressionDepth(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id) VALUES('1',1)`); err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 2000)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	deleted, err := a.deleteRows(httptest.NewRequest("POST", "/", nil), ids, nil)
	if err != nil || deleted != 1 {
		t.Fatalf("2000-item batch: deleted %d error %v", deleted, err)
	}
}

func TestMediaReadAndDeleteCannotFollowOutsideSymlink(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	outside := t.TempDir()
	file := filepath.Join(outside, "secret.jpg")
	if err := os.WriteFile(file, []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(a.dataDir, "downloads")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skip(err)
	}
	token, err := a.sessions.Create(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/files/escape/secret.jpg", nil)
	r.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code == 200 || strings.Contains(w.Body.String(), "private fixture") {
		t.Error("outside file served")
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_path) VALUES('1',1,'escape/secret.jpg')`); err != nil {
		t.Fatal(err)
	}
	_, _ = a.deleteRows(r, []int64{1}, nil)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("outside file deleted: %v", err)
	}
}

func TestPurgeRequiresExactConfirmation(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id) VALUES('1',1)`); err != nil {
		t.Fatal(err)
	}
	token, err := a.sessions.Create(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", `{}`, `{"confirm":true}`, `{"confirm":"delete all"}`} {
		r := httptest.NewRequest("DELETE", "/api/purge/all", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("unconfirmed purge status %d", w.Code)
		}
	}
	var count int
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unconfirmed purge mutated rows: %d %v", count, err)
	}
}

func TestCleanupFailureSurvivesRestartAndRetries(t *testing.T) {
	dir := t.TempDir()
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			a.Close()
		}
	}()
	file := filepath.Join(dir, "downloads", "blocked.jpg")
	if err := os.MkdirAll(file, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, "child"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_path) VALUES('1',1,'blocked.jpg')`); err != nil {
		t.Fatal(err)
	}
	n, err := a.deleteRows(httptest.NewRequest("POST", "/", nil), []int64{1}, nil)
	if n != 1 || err == nil {
		t.Fatalf("delete=%d error=%v; expected cleanup failure", n, err)
	}
	var pending int
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_file_cleanup`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("lost cleanup task: %d %v", pending, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = nil
	if err := os.Remove(filepath.Join(file, "child")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("ready to remove"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err = New(context.Background(), Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("startup did not retry cleanup: %v", err)
	}
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_file_cleanup`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("completed cleanup remains: %d %v", pending, err)
	}
}
