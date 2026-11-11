package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/backup"
)

type rangeFixtureProvider struct {
	data  []byte
	err   error
	calls atomic.Int64
	wait  <-chan struct{}
	enter chan<- struct{}
}

func (p *rangeFixtureProvider) Test(context.Context) (string, error) { return "ok", nil }
func (p *rangeFixtureProvider) Upload(context.Context, string, io.ReadSeeker, int64, func(int64)) (backup.UploadResult, error) {
	return backup.UploadResult{}, nil
}
func (p *rangeFixtureProvider) Download(context.Context, string, io.Writer) error {
	return errors.New("whole-file download called")
}
func (p *rangeFixtureProvider) ReadRange(_ context.Context, _ string, start, length int64) ([]byte, int64, error) {
	p.calls.Add(1)
	if p.enter != nil {
		select {
		case p.enter <- struct{}{}:
		default:
		}
	}
	if p.wait != nil {
		<-p.wait
	}
	if p.err != nil {
		return nil, 0, p.err
	}
	end := min(start+length, int64(len(p.data)))
	if start < 0 || start >= end {
		return nil, int64(len(p.data)), errors.New("bad range")
	}
	return append([]byte(nil), p.data[start:end]...), int64(len(p.data)), nil
}
func (p *rangeFixtureProvider) List(context.Context, string) ([]backup.Object, error) {
	return nil, nil
}
func (p *rangeFixtureProvider) Delete(context.Context, string) error { return nil }
func (p *rangeFixtureProvider) Close() error                         { return nil }

func TestParseMediaRange(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  mediaByteRange
		ok    bool
	}{
		{"bytes=2-5", mediaByteRange{2, 5}, true},
		{"bytes=8-", mediaByteRange{8, 9}, true},
		{"bytes=-3", mediaByteRange{7, 9}, true},
		{"bytes=10-", mediaByteRange{}, false},
		{"bytes=5-2", mediaByteRange{}, false},
		{"bytes=0-1,3-4", mediaByteRange{}, false},
	} {
		got, ok := parseMediaRange(tc.value, 10)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseMediaRange(%q) = %+v,%v", tc.value, got, ok)
		}
	}
}

func TestDriveBackedMediaServesRangesFromVerifiedMirror(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.backups.Close()
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	provider := &rangeFixtureProvider{data: data}
	m, err := backup.NewManager(a.ctx, backup.Options{
		Writer: a.db.Writer, Reader: a.db.Reader, DataDir: a.dataDir,
		Secret: func(context.Context) ([]byte, error) { return make([]byte, 32), nil },
		Factory: func(context.Context, string, map[string]any) (backup.Provider, error) {
			return provider, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.backups = m
	dest, err := m.Create(ctx, map[string]any{"name": "Drive", "provider": "gdrive", "enabled": false, "mode": "mirror"})
	if err != nil {
		t.Fatal(err)
	}
	destinationID := dest["id"].(int64)
	if _, err = a.db.Writer.Exec(`UPDATE backup_destinations SET enabled=1 WHERE id=?`, destinationID); err != nil {
		t.Fatal(err)
	}
	res, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_hash,remote_destination_id) VALUES('g',1,'clip.mp4',?,'video','sha256',?)`, len(data), destinationID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Writer.Exec(`INSERT INTO backup_jobs(destination_id,download_id,status,remote_path,bytes_uploaded) VALUES(?,?,'done','clip.mp4',?)`, destinationID, id, len(data)); err != nil {
		t.Fatal(err)
	}
	token, err := a.sessions.Create(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated := httptest.NewRecorder()
	a.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/files/clip.mp4?inline=1", nil))
	if unauthenticated.Code != http.StatusFound {
		t.Fatalf("Drive media route bypassed authentication: status=%d", unauthenticated.Code)
	}
	localBytes := []byte("local copy has priority")
	localPath := filepath.Join(a.downloadsDir, "clip.mp4")
	if err = os.WriteFile(localPath, localBytes, 0600); err != nil {
		t.Fatal(err)
	}
	localReq := httptest.NewRequest(http.MethodGet, "/files/clip.mp4?inline=1", nil)
	localReq.Header.Set("Range", "bytes=2-8")
	localReq.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	localResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(localResponse, localReq)
	if localResponse.Code != http.StatusPartialContent || localResponse.Body.String() != string(localBytes[2:9]) {
		t.Fatalf("local file did not take precedence: status=%d body=%q", localResponse.Code, localResponse.Body.String())
	}
	if err = os.Remove(localPath); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/files/clip.mp4?inline=1", nil)
	req.Header.Set("Range", "bytes=2-8")
	req.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusPartialContent || response.Body.String() != string(data[2:9]) || response.Header().Get("Content-Range") != "bytes 2-8/36" {
		t.Fatalf("remote range response: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	ifRange := httptest.NewRequest(http.MethodGet, "/files/clip.mp4?inline=1", nil)
	ifRange.Header.Set("Range", "bytes=2-8")
	ifRange.Header.Set("If-Range", `"stale"`)
	ifRange.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	ifRangeResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(ifRangeResponse, ifRange)
	if ifRangeResponse.Code != http.StatusOK || ifRangeResponse.Body.String() != string(data) {
		t.Fatalf("If-Range mismatch response: status=%d body=%q", ifRangeResponse.Code, ifRangeResponse.Body.String())
	}
	if !strings.HasPrefix(response.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("disposition=%q", response.Header().Get("Content-Disposition"))
	}
	statsDenied := httptest.NewRecorder()
	a.Handler().ServeHTTP(statsDenied, httptest.NewRequest(http.MethodGet, "/api/backup/media/stats", nil))
	if statsDenied.Code != http.StatusUnauthorized {
		t.Fatalf("stream stats are not protected: status=%d", statsDenied.Code)
	}
	statsRequest := httptest.NewRequest(http.MethodGet, "/api/backup/media/stats", nil)
	statsRequest.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	statsResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(statsResponse, statsRequest)
	var streamStats struct {
		Stream map[string]int64 `json:"stream"`
	}
	if statsResponse.Code != http.StatusOK || json.Unmarshal(statsResponse.Body.Bytes(), &streamStats) != nil || streamStats.Stream["bytesServed"] < 7 || streamStats.Stream["firstByteSamples"] == 0 || streamStats.Stream["p50FirstByteNanos"] == 0 || streamStats.Stream["p95FirstByteNanos"] == 0 {
		t.Fatalf("admin stream stats: status=%d body=%s", statsResponse.Code, statsResponse.Body.String())
	}
	gallery, total, err := a.queryGalleryFiles(httptest.NewRequest(http.MethodGet, "/api/downloads/all", nil), galleryQuery{limit: 20})
	if err != nil || total != 1 || len(gallery) != 1 || gallery[0]["remoteOnly"] != true || gallery[0]["fullPath"] != "clip.mp4" {
		t.Fatalf("remote gallery item=%v total=%d err=%v", gallery, total, err)
	}
	if _, err = os.Stat(filepath.Join(a.dataDir, "cache", "gdrive-range")); !os.IsNotExist(err) {
		t.Fatalf("small file was cached whole: %v", err)
	}
	head := httptest.NewRequest(http.MethodHead, "/files/clip.mp4?inline=1", nil)
	head.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	headResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(headResponse, head)
	if headResponse.Code != http.StatusOK || headResponse.Body.Len() != 0 || headResponse.Header().Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("HEAD status=%d headers=%v body=%q", headResponse.Code, headResponse.Header(), headResponse.Body.String())
	}
	badRange := httptest.NewRequest(http.MethodGet, "/files/clip.mp4?inline=1", nil)
	badRange.Header.Set("Range", "bytes=99-")
	badRange.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	badResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(badResponse, badRange)
	if badResponse.Code != http.StatusRequestedRangeNotSatisfiable || badResponse.Header().Get("Content-Range") != "bytes */36" {
		t.Fatalf("invalid range: %d %v", badResponse.Code, badResponse.Header())
	}
	provider.err = errors.New("Drive is offline")
	driveFailure := httptest.NewRequest(http.MethodGet, "/files/clip.mp4?inline=1", nil)
	driveFailure.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	driveFailureResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(driveFailureResponse, driveFailure)
	var remaining int
	if err = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE id=? AND remote_destination_id=?`, id, destinationID).Scan(&remaining); err != nil || remaining != 1 || driveFailureResponse.Code != http.StatusBadGateway {
		t.Fatalf("Drive failure removed source row: status=%d remaining=%d err=%v", driveFailureResponse.Code, remaining, err)
	}
	provider.err = nil
	provider.data = make([]byte, 40<<20)
	sequentialID, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_hash,remote_destination_id) VALUES('g',2,'sequential.mp4',?,'video','sequential-sha256',?)`, len(provider.data), destinationID)
	if err != nil {
		t.Fatal(err)
	}
	sequentialDownloadID, _ := sequentialID.LastInsertId()
	if _, err = a.db.Writer.Exec(`INSERT INTO backup_jobs(destination_id,download_id,status,remote_path,bytes_uploaded) VALUES(?,?,'done','sequential.mp4',?)`, destinationID, sequentialDownloadID, len(provider.data)); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	cacheDir := filepath.Join(a.dataDir, "cache", "gdrive-range")
	for start := int64(0); start < int64(len(provider.data)); start += driveMediaBlockSize {
		if _, err = a.driveMediaBlock(request, destinationID, "sequential.mp4", "sequential-sha256", start, int64(len(provider.data))); err != nil {
			t.Fatalf("sequential read at %d: %v", start, err)
		}
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("one-pass sequential playback wrote cache blocks: %v", entries)
	}
	largeID, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_hash,remote_destination_id) VALUES('g',3,'large.mp4',?,'video','large-sha256',?)`, len(provider.data), destinationID)
	if err != nil {
		t.Fatal(err)
	}
	largeDownloadID, _ := largeID.LastInsertId()
	if _, err = a.db.Writer.Exec(`INSERT INTO backup_jobs(destination_id,download_id,status,remote_path,bytes_uploaded) VALUES(?,?,'done','large.mp4',?)`, destinationID, largeDownloadID, len(provider.data)); err != nil {
		t.Fatal(err)
	}
	before := provider.calls.Load()
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	provider.wait, provider.enter = gate, entered
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, e := a.driveMediaBlock(request, destinationID, "large.mp4", "large-sha256", 0, int64(len(provider.data))); e != nil {
				t.Errorf("coalesced range read: %v", e)
			}
		}()
	}
	close(start)
	<-entered
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	provider.wait, provider.enter = nil, nil
	if got := provider.calls.Load() - before; got != 1 {
		t.Fatalf("concurrent identical reads made %d Drive calls", got)
	}
	entries, err = os.ReadDir(cacheDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("first-use range was written to disk: %v", entries)
	}
	if _, err = a.driveMediaBlock(request, destinationID, "large.mp4", "large-sha256", 0, int64(len(provider.data))); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(cacheDir)
	if (err != nil && !os.IsNotExist(err)) || len(entries) != 0 {
		t.Fatalf("one subsequent read admitted a range too early: entries=%v err=%v", entries, err)
	}
	if _, err = a.driveMediaBlock(request, destinationID, "large.mp4", "large-sha256", 0, int64(len(provider.data))); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(cacheDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("reused range not admitted to cache: entries=%v err=%v", entries, err)
	}
	before = provider.calls.Load()
	if _, err = a.driveMediaBlock(request, destinationID, "large.mp4", "large-sha256", 0, int64(len(provider.data))); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != before {
		t.Fatal("admitted range was fetched from Drive again")
	}
	gate, entered = make(chan struct{}), make(chan struct{}, 1)
	provider.wait, provider.enter = gate, entered
	leaderDone := make(chan error, 1)
	go func() {
		_, e := a.driveMediaBlock(request, destinationID, "large.mp4", "large-sha256", driveMediaBlockSize, int64(len(provider.data)))
		leaderDone <- e
	}()
	<-entered
	waiterCtx, cancelWaiter := context.WithCancel(ctx)
	waiterDone := make(chan error, 1)
	go func() {
		_, e := a.driveMediaBlock(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(waiterCtx), destinationID, "large.mp4", "large-sha256", driveMediaBlockSize, int64(len(provider.data)))
		waiterDone <- e
	}()
	time.Sleep(20 * time.Millisecond)
	cancelWaiter()
	select {
	case e := <-waiterDone:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancelled waiter error=%v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter kept waiting for the shared Drive read")
	}
	close(gate)
	if err = <-leaderDone; err != nil {
		t.Fatalf("shared read leader failed after waiter cancellation: %v", err)
	}
	provider.wait, provider.enter = nil, nil
}

func TestDriveMediaCachePrunesLeastRecentlyUsedBlocks(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	dir := filepath.Join(a.dataDir, "cache", "gdrive-range")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		file := filepath.Join(dir, strconv.Itoa(i))
		f, e := os.Create(file)
		if e != nil {
			t.Fatal(e)
		}
		if e = f.Truncate(1 << 20); e != nil {
			f.Close()
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
		stamp := time.Unix(int64(i+1), 0)
		_ = os.Chtimes(file, stamp, stamp)
	}
	a.pruneDriveMediaCache(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		info, e := entry.Info()
		if e != nil {
			t.Fatal(e)
		}
		total += info.Size()
	}
	if total > driveMediaCacheLimit {
		t.Fatalf("cache grew to %d bytes", total)
	}
}

func TestLocalOnlyMaintenanceQueuesSkipDriveOnlyMedia(t *testing.T) {
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_path,file_size,file_type,remote_destination_id) VALUES
		('g',1,'remote.mp4',NULL,100,'video',42),('g',2,'local.mp4','local.mp4',100,'video',NULL),('g',3,'remote.jpg',NULL,100,'photo',42),('g',4,'local.jpg','local.jpg',100,'photo',NULL)`); err != nil {
		t.Fatal(err)
	}
	rows, err := a.videoRows(context.Background())
	if err != nil || len(rows) != 1 || rows[0].path != "local.mp4" {
		t.Fatalf("faststart rows=%+v err=%v", rows, err)
	}
	thumbs, err := a.thumbRows(context.Background(), "all")
	if err != nil || len(thumbs) != 2 {
		t.Fatalf("thumbnail rows=%+v err=%v", thumbs, err)
	}
	faces, err := a.faces.unindexed(context.Background(), []string{"photo", "video"}, 10)
	if err != nil || len(faces) != 2 {
		t.Fatalf("face rows=%+v err=%v", faces, err)
	}
	faceCount, err := a.faces.countUnindexed(context.Background(), []string{"photo", "video"})
	if err != nil || faceCount != 2 {
		t.Fatalf("face count=%d err=%v", faceCount, err)
	}
	nsfwRows, err := a.nsfwBatch(context.Background(), "?,?", nsfwConfig{BatchSize: 10, FileTypes: []string{"video", "photo"}}, 0)
	if err != nil || len(nsfwRows) != 2 {
		t.Fatalf("NSFW rows=%+v err=%v", nsfwRows, err)
	}
}
