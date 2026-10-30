package app

import (
	"context"
	"database/sql"
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

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type sentFile struct {
	account string
	req     telegram.SendFileRequest
	body    string
}

type fakeForwarder struct {
	observing      atomic.Bool
	mu             sync.Mutex
	sent           []sentFile
	forwardRequest []telegram.ForwardMessageRequest
	err            error
}

func (f *fakeForwarder) Observing() bool { return f.observing.Load() }

func (f *fakeForwarder) SendFile(_ context.Context, account string, req telegram.SendFileRequest) (string, int, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return "", 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentFile{account: account, req: req, body: string(body)})
	if account == "" {
		account = "default"
	}
	return account, 41, f.err
}
func (f *fakeForwarder) ForwardMessage(_ context.Context, account string, req telegram.ForwardMessageRequest) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forwardRequest = append(f.forwardRequest, req)
	if req.ResultMessageID != nil {
		*req.ResultMessageID = 42
	}
	if account == "" {
		account = "default"
	}
	if !req.EditCaption {
		f.sent = append(f.sent, sentFile{account: account, req: telegram.SendFileRequest{Destination: req.Destination}})
	}
	return account, 42, f.err
}

func (f *fakeForwarder) calls() []sentFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentFile(nil), f.sent...)
}

func (f *fakeForwarder) forwards() []telegram.ForwardMessageRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telegram.ForwardMessageRequest(nil), f.forwardRequest...)
}

func (f *fakeForwarder) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func forwardTestApp(t *testing.T, autoForward map[string]any) (*App, *fakeForwarder) {
	t.Helper()
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir(), Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["groups"] = []any{
		map[string]any{"id": "-1000000000123", "name": "Source", "enabled": true, "autoForward": autoForward, "forwardAccount": "acct2"},
		map[string]any{"id": "-1000000000456", "name": "Other", "enabled": true},
	}
	if err := a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	fake := &fakeForwarder{}
	fake.observing.Store(true)
	a.forwarder = fake
	return a, fake
}

func TestForwardProtectionInheritanceAndCaptionMethod(t *testing.T) {
	cfg := map[string]any{"forwarding": map[string]any{"protect": true}}
	group := map[string]any{"protect": true}
	route := map[string]any{"captionMode": "native", "protect": false}
	settings := effectiveForwardProtection(route, group, route, 7, cfg)
	if settings["protect"] != true {
		t.Fatalf("group override lost: %#v", settings)
	}
	if customCaption(settings) {
		t.Fatal("source caption should permit native forwarding")
	}
	settings = effectiveForwardProtection(map[string]any{"protect": false}, group, route, 7, cfg)
	if settings["protect"] != true {
		t.Fatalf("legacy route override replaced group rule: %#v", settings)
	}
	if customCaption(map[string]any{"captionMode": "native"}) {
		t.Fatal("native caption should permit direct forwarding")
	}
	if normalized, err := normalizedAutoForward(nil, map[string]any{"directForward": false}); err != nil || normalized["directForward"] != false {
		t.Fatalf("direct forward opt-out was not preserved: %#v, %v", normalized, err)
	}
}

func TestAutoForwardWithoutDownloadRequiresDeleteAfterForward(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{"enabled": true, "destination": "@Archive", "deleteAfterForward": true, "directForward": true, "captionMode": "native"})
	var directCalls atomic.Int32
	transport := mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		_, err := w.Write([]byte("media"))
		return err
	})
	_, err := a.ingestTelegramWithForward(ctx, docMessage(10, 123, 77, "native caption"), "", "Source", "acct1", transport, func(_ context.Context, request telegram.ForwardMessageRequest) error {
		if request.Destination != "@Archive" || request.MessageID != 10 {
			t.Fatalf("forward request=%+v", request)
		}
		directCalls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "native forward", func() bool { return directCalls.Load() == 1 })
	if len(fake.calls()) != 0 {
		t.Fatalf("unexpected uploads=%d", len(fake.calls()))
	}
	eventually(t, "local copy deletion", func() bool {
		_, exists := rowPath(t, a, "-1000000000123", 10)
		return !exists
	})
	if _, exists := rowPath(t, a, "-1000000000123", 10); exists {
		t.Fatal("direct-forwarded media was saved locally")
	}

	// With delete-after-forward off, the message must use the download/upload path.
	a.configMu.Lock()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		a.configMu.Unlock()
		t.Fatal(err)
	}
	groups, _ := cfg["groups"].([]any)
	group := groups[0].(map[string]any)
	settings := group["autoForward"].(map[string]any)
	settings["deleteAfterForward"] = false
	group["autoForward"] = settings
	cfg["groups"] = groups
	err = a.config.Save(ctx, cfg)
	a.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	transport = mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		downloads.Add(1)
		_, writeErr := w.Write([]byte("media"))
		return writeErr
	})
	_, err = a.ingestTelegramWithForward(ctx, docMessage(11, 123, 78, ""), "", "Source", "acct1", transport, func(context.Context, telegram.ForwardMessageRequest) error {
		directCalls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "uploaded downloaded media", func() bool { return len(fake.calls()) == 1 })
	if downloads.Load() != 1 || directCalls.Load() != 1 {
		t.Fatalf("downloads=%d direct=%d", downloads.Load(), directCalls.Load())
	}
	if _, exists := rowPath(t, a, "-1000000000123", 11); !exists {
		t.Fatal("downloaded media not retained when delete-after-forward is off")
	}
}

func TestAutoForwardReturnsBeforeNativeForwardCompletes(t *testing.T) {
	ctx := context.Background()
	a, _ := forwardTestApp(t, map[string]any{"enabled": true, "destination": "@Archive", "deleteAfterForward": true, "directForward": true, "captionMode": "native"})
	started, release := make(chan struct{}), make(chan struct{})
	transport := mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		_, err := w.Write([]byte("media"))
		return err
	})
	done := make(chan error, 1)
	go func() {
		_, err := a.ingestTelegramWithForward(ctx, docMessage(901, 123, 99, "native caption"), "", "Source", "acct1", transport, func(context.Context, telegram.ForwardMessageRequest) error {
			close(started)
			<-release
			return nil
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native forward did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ingest waited for native forward instead of queueing it")
	}
	close(release)
	eventually(t, "download finishing", func() bool {
		var status string
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT outcome FROM tgdl_processing_attempts WHERE message_id=901 ORDER BY id DESC LIMIT 1`).Scan(&status)
		return status == "downloaded"
	})
}

func TestCustomCaptionUsesDirectForwardThenEdit(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{
		"enabled": true, "destination": "@Archive", "deleteAfterForward": true,
		"directForward": true, "captionMode": "custom", "captionBase": "custom",
		"caption": "Hi {{source_name}} #{{message_id}}",
	})
	transport := mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		_, err := w.Write([]byte("media"))
		return err
	})
	requestCh := make(chan telegram.ForwardMessageRequest, 1)
	_, err := a.ingestTelegramWithForward(ctx, docMessage(13, 123, 80, "source caption"), "", "Source", "acct1", transport, func(_ context.Context, req telegram.ForwardMessageRequest) error {
		requestCh <- req
		if req.ResultMessageID != nil {
			*req.ResultMessageID = 42
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var request telegram.ForwardMessageRequest
	select {
	case request = <-requestCh:
	case <-time.After(time.Second):
		t.Fatal("native forward did not start")
	}
	if !request.EditCaption || request.Caption != "Hi Source #13" {
		t.Fatalf("native forward request=%+v", request)
	}
	if len(fake.calls()) != 0 {
		t.Fatalf("unexpected upload calls: %d", len(fake.calls()))
	}
	eventually(t, "local copy deletion", func() bool {
		_, exists := rowPath(t, a, "-1000000000123", 13)
		return !exists
	})
	if _, exists := rowPath(t, a, "-1000000000123", 13); exists {
		t.Fatal("direct-forwarded media was saved locally")
	}
}

func TestUnresolvedCustomCaptionForwardFailsWithoutDuplicateUpload(t *testing.T) {
	ctx := context.Background()
	a, _ := forwardTestApp(t, map[string]any{
		"enabled": true, "destination": "@Archive", "deleteAfterForward": true,
		"directForward": true, "captionMode": "custom", "captionBase": "custom", "caption": "custom",
	})
	transport := mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		_, err := w.Write([]byte("media"))
		return err
	})
	_, err := a.ingestTelegramWithForward(ctx, docMessage(14, 123, 81, "source caption"), "", "Source", "acct1", transport, func(_ context.Context, request telegram.ForwardMessageRequest) error {
		if request.ResultMessageID != nil {
			*request.ResultMessageID = 9001
		}
		return telegram.ErrForwardCaptionUnresolved
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	eventually(t, "caption failure recorded", func() bool {
		var status string
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT forward_status FROM tgdl_processing_attempts WHERE message_id=14 ORDER BY id DESC LIMIT 1`).Scan(&status)
		return status == "failed"
	})
	var status, stage, states string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT status,stage,destinations_json FROM tgdl_forward_queue ORDER BY id DESC LIMIT 1`).Scan(&status, &stage, &states); err != nil {
		t.Fatal(err)
	}
	var destinations []forwardDestination
	if err := json.Unmarshal([]byte(states), &destinations); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || stage != "caption_edit_failed" || len(destinations) != 1 || !destinations[0].DirectFailed || destinations[0].DestinationMessageID != 9001 {
		t.Fatalf("status=%q stage=%q destinations=%+v", status, stage, destinations)
	}
}

func TestRetryCaptionEditUsesSavedDestinationMessageID(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{
		"enabled": true, "destination": "@Archive", "deleteAfterForward": true,
		"directForward": true, "captionMode": "custom", "captionBase": "custom", "caption": "fixed caption",
	})
	settings := map[string]any{
		"enabled": true, "destination": "@Archive", "deleteAfterForward": true,
		"directForward": true, "captionMode": "custom", "captionBase": "custom", "caption": "fixed caption",
	}
	a.forward(ctx, forwardJob{
		groupID: "-1000000000123", groupName: "Source", messageID: 14, text: "source caption",
		settings: settings, stage: "caption_edit_failed", path: "already-downloaded.mp4",
		destinations: []forwardDestination{{
			Settings: map[string]any{"destination": "@Archive", "captionMode": "custom", "captionBase": "custom", "caption": "fixed caption"},
			Status:   "processing", DirectFailed: true, DestinationMessageID: 9001,
		}},
	})
	requests := fake.forwards()
	if len(requests) != 1 || requests[0].EditMessageID != 9001 || !requests[0].EditCaption || requests[0].MessageID != 14 || len(fake.calls()) != 0 {
		t.Fatalf("retry requests=%+v calls=%+v", requests, fake.calls())
	}
}

func TestForwardQueueRetryResetsFailedAndInterruptedJobs(t *testing.T) {
	ctx := context.Background()
	a, _ := forwardTestApp(t, map[string]any{"enabled": true, "destination": "@Archive"})
	for _, status := range []string{"failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			var id int64
			err := a.db.Writer.QueryRowContext(ctx, `INSERT INTO tgdl_forward_queue(group_id,group_name,message_id,status,reason,stage,destinations_json,created_at,updated_at,completed_at)
				VALUES('-1000000000123','Source',14,?,'temporary failure','upload','[{"status":"forwarded"}]',1,1,1) RETURNING id`, status).Scan(&id)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/forward-queue/1/retry", nil)
			r.SetPathValue("id", strconv.FormatInt(id, 10))
			w := httptest.NewRecorder()
			a.handleForwardQueueRetry(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("retry status=%d body=%s", w.Code, w.Body.String())
			}
			var gotStatus, reason, stage, destinations string
			var completedAt int64
			if err := a.db.Reader.QueryRowContext(ctx, `SELECT status,reason,stage,destinations_json,completed_at FROM tgdl_forward_queue WHERE id=?`, id).Scan(&gotStatus, &reason, &stage, &destinations, &completedAt); err != nil {
				t.Fatal(err)
			}
			if gotStatus != "pending" || reason != "" || stage != "upload" || destinations != `[{"status":"forwarded"}]` || completedAt != 0 {
				t.Fatalf("retry state=%q reason=%q stage=%q destinations=%s completedAt=%d", gotStatus, reason, stage, destinations, completedAt)
			}
		})
	}
}

func TestUnresolvedCustomCaptionWithoutMessageIDDoesNotRetryAsNewForward(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{
		"enabled": true, "destination": "@Archive", "deleteAfterForward": true,
		"directForward": true, "captionMode": "custom", "captionBase": "custom", "caption": "fixed caption",
	})
	a.forward(ctx, forwardJob{
		groupID: "-1000000000123", groupName: "Source", messageID: 14,
		settings: map[string]any{
			"enabled": true, "destination": "@Archive", "deleteAfterForward": true,
			"directForward": true, "captionMode": "custom", "captionBase": "custom", "caption": "fixed caption",
		},
		stage: "caption_edit_failed",
		path:  "already-downloaded.mp4",
		destinations: []forwardDestination{{
			Settings: map[string]any{"destination": "@Archive", "captionMode": "custom", "captionBase": "custom", "caption": "fixed caption"},
			Status:   "processing", DirectFailed: true,
		}},
	})
	if len(fake.forwards()) != 0 {
		t.Fatalf("unsafe retry created another forward: %+v", fake.forwards())
	}
}

func TestDirectForwardFailureDownloadsThenUploads(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{"enabled": true, "destination": "@Archive", "deleteAfterForward": true, "directForward": true, "captionMode": "native"})
	var directCalls, downloads atomic.Int32
	transport := mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		downloads.Add(1)
		_, err := w.Write([]byte("media"))
		return err
	})
	_, err := a.ingestTelegramWithForward(ctx, docMessage(12, 123, 79, "source caption"), "", "Source", "acct1", transport, func(context.Context, telegram.ForwardMessageRequest) error {
		directCalls.Add(1)
		return errors.New("Telegram forbids native forwarding")
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "fallback upload", func() bool { return len(fake.calls()) == 1 })
	calls := fake.calls()
	if directCalls.Load() != 1 || downloads.Load() != 1 {
		t.Fatalf("direct attempts=%d downloads=%d", directCalls.Load(), downloads.Load())
	}
	if calls[0].body != "media" || calls[0].req.Protect {
		t.Fatalf("fallback upload=%+v body=%q", calls[0].req, calls[0].body)
	}
	eventually(t, "fallback local copy deletion", func() bool {
		_, exists := rowPath(t, a, "-1000000000123", 12)
		return !exists
	})
	if _, exists := rowPath(t, a, "-1000000000123", 12); exists {
		t.Fatal("fallback download was retained despite delete-after-forward")
	}
}

func TestProtectedAutoForwardDownloadsAndUploadsProtectedCopy(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{"enabled": true, "destination": "@Archive", "deleteAfterForward": true, "protect": true, "captionMode": "native"})
	if _, err := a.ingestTelegramWithForward(ctx, docMessage(20, 123, 88, ""), "", "Source", "acct1", fiveBytes, func(context.Context, telegram.ForwardMessageRequest) error {
		t.Fatal("protected media used native forwarding")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "protected upload", func() bool { return len(fake.calls()) == 1 })
	call := fake.calls()[0]
	if call.body != "media" || !call.req.Protect {
		t.Fatalf("protected upload=%+v", call)
	}
	if _, exists := rowPath(t, a, "-1000000000123", 20); exists {
		t.Fatal("protected copy should delete local media after successful forward")
	}
}

func docMessage(id int, channel, doc int64, text string) *tg.Message {
	return &tg.Message{ID: id, Message: text, PeerID: &tg.PeerChannel{ChannelID: channel}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: doc, Size: 5, DCID: 2, Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "clip.bin"}}}}}
}

var fiveBytes = mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
	_, err := w.Write([]byte("media"))
	return err
})

func rowPath(t *testing.T, a *App, group string, message int) (string, bool) {
	t.Helper()
	var path string
	err := a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE group_id=? AND message_id=?`, group, message).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return path, true
}

func TestAutoForwardUploadsAndDeletesOnlyTheLocalCopy(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{"enabled": true, "destination": "@Archive", "deleteAfterForward": true})
	// Another chat already holds the same media: the forwarded message reuses
	// its file, which must survive deleteAfterForward.
	if _, err := a.ingestTelegram(ctx, docMessage(1, 456, 77, ""), "", "Other", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	shared, _ := rowPath(t, a, "-1000000000456", 1)
	if _, err := a.ingestTelegram(ctx, docMessage(10, 123, 77, "caption text"), "", "Source", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	eventually(t, "forwarded row removal", func() bool { _, ok := rowPath(t, a, "-1000000000123", 10); return !ok })
	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("calls=%d", len(calls))
	}
	call := calls[0]
	if call.account != "acct2" || call.req.Destination != "@Archive" || call.body != "media" || call.req.Size != 5 {
		t.Fatalf("send=%+v", call)
	}
	if call.req.Caption != "caption text\n\n📌 Source: Source" || len(call.req.Entities) != 1 {
		t.Fatalf("caption=%q", call.req.Caption)
	}
	if link, ok := call.req.Entities[0].(*tg.MessageEntityTextURL); !ok || link.URL != "https://t.me/c/123/10" {
		t.Fatalf("entity=%#v", call.req.Entities[0])
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, shared)); err != nil {
		t.Fatalf("shared file removed: %v", err)
	}
	if _, ok := rowPath(t, a, "-1000000000456", 1); !ok {
		t.Fatal("another chat's row was deleted")
	}

	// A file only this message owns is removed from disk.
	unique := mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		_, err := w.Write([]byte("other"))
		return err
	})
	// The forward worker may already have removed the row; take the path from
	// the ingest result instead of reading it back.
	record, err := a.ingestTelegram(ctx, docMessage(11, 123, 78, ""), "", "Source", "acct1", unique)
	if err != nil {
		t.Fatal(err)
	}
	own := record.Path
	if own == "" {
		t.Fatal("download missing")
	}
	eventually(t, "local file removal", func() bool {
		_, err := os.Stat(filepath.Join(a.downloadsDir, own))
		return os.IsNotExist(err)
	})
	if len(fake.calls()) != 2 {
		t.Fatalf("calls=%d", len(fake.calls()))
	}
}

func TestAutoForwardSkipsWhenNotLiveDisabledOrAlreadyDownloaded(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{"enabled": true, "destination": "me", "deleteAfterForward": false})
	fake.observing.Store(false)
	if _, err := a.ingestTelegram(ctx, docMessage(1, 123, 1, ""), "", "Source", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	fake.observing.Store(true)
	// Re-ingesting a message already in the library is not a new download.
	if _, err := a.ingestTelegram(ctx, docMessage(1, 123, 1, ""), "", "Source", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ingestTelegram(ctx, docMessage(2, 456, 2, ""), "", "Other", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ingestTelegram(ctx, docMessage(3, 123, 3, ""), "", "Source", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	eventually(t, "forward", func() bool { return len(fake.calls()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if calls := fake.calls(); len(calls) != 1 || !strings.HasPrefix(calls[0].req.Caption, "📌 Source: ") {
		t.Fatalf("calls=%+v", calls)
	}
	// deleteAfterForward off: the download stays.
	if _, ok := rowPath(t, a, "-1000000000123", 3); !ok {
		t.Fatal("download removed without deleteAfterForward")
	}
}

func TestQueuedAutoForwardStopsWhenDisabledAfterEnqueue(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{"enabled": false, "destination": "me"})
	a.forward(ctx, forwardJob{
		groupID: "-1000000000123",
		settings: map[string]any{
			"enabled": true,
			"routes":  []any{map[string]any{"sourceTopicId": float64(0), "destinationGroupId": "me"}},
		},
		stage: "metadata",
	})
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("disabled auto-forward still sent %d messages", len(calls))
	}
}

func TestAutoForwardDoesNotUploadFallbackAfterItIsDisabled(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{
		"enabled": true, "destination": "@Archive", "deleteAfterForward": true, "captionMode": "native",
	})
	transport := mediaTransport(func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
		_, err := w.Write([]byte("media"))
		return err
	})
	_, err := a.ingestTelegramWithForward(ctx, docMessage(21, 123, 87, ""), "", "Source", "acct1", transport, func(context.Context, telegram.ForwardMessageRequest) error {
		a.configMu.Lock()
		defer a.configMu.Unlock()
		cfg, err := a.config.Load(ctx)
		if err != nil {
			return err
		}
		groups, _ := cfg["groups"].([]any)
		group := groups[0].(map[string]any)
		settings := group["autoForward"].(map[string]any)
		settings["enabled"] = false
		group["autoForward"] = settings
		cfg["groups"] = groups
		if err := a.config.Save(ctx, cfg); err != nil {
			return err
		}
		return errors.New("native forwarding failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("auto-forward uploaded after disable: %d uploads", len(calls))
	}
	if _, exists := rowPath(t, a, "-1000000000123", 21); !exists {
		t.Fatal("source media should still be downloaded after native forwarding fails")
	}
}

func TestAutoForwardPausesARefusingDestinationUntilItsRecheck(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, map[string]any{"enabled": true, "destination": "@Archive", "deleteAfterForward": true})
	fake.fail(tgerr.New(403, "CHAT_WRITE_FORBIDDEN"))
	if _, err := a.ingestTelegram(ctx, docMessage(1, 123, 1, ""), "", "Source", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	var state, accounts string
	var next int64
	eventually(t, "destination access row", func() bool {
		return a.db.Reader.QueryRow(`SELECT state,next_check_at,accounts FROM chat_access WHERE chat_id='dest:@archive'`).Scan(&state, &next, &accounts) == nil
	})
	if state != "restricted" || next <= time.Now().UnixMilli() || !strings.Contains(accounts, `"acct2"`) {
		t.Fatalf("state=%s next=%d accounts=%s", state, next, accounts)
	}
	// A failed forward never deletes anything.
	if _, ok := rowPath(t, a, "-1000000000123", 1); !ok {
		t.Fatal("download deleted after a failed forward")
	}
	if _, err := a.ingestTelegram(ctx, docMessage(2, 123, 2, ""), "", "Source", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("paused destination still received %d uploads", n)
	}
	token, err := a.sessions.Create(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/groups", nil)
	req.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	var groups []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	if access, ok := groups[0]["forwardAccess"].(map[string]any); !ok || access["state"] != "restricted" {
		t.Fatalf("group=%v", groups[0])
	}

	// Its re-check time comes: the next forward is the re-check, and a
	// success clears the pause.
	fake.fail(nil)
	if _, err := a.db.Writer.Exec(`UPDATE chat_access SET next_check_at=1 WHERE chat_id='dest:@archive'`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ingestTelegram(ctx, docMessage(3, 123, 3, ""), "", "Source", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	eventually(t, "access row cleared", func() bool {
		var n int
		return a.db.Reader.QueryRow(`SELECT COUNT(*) FROM chat_access WHERE chat_id='dest:@archive'`).Scan(&n) == nil && n == 0
	})
	if n := len(fake.calls()); n != 2 {
		t.Fatalf("calls=%d", n)
	}
}

func TestRecordDestinationAccessBacksOffOnRechecks(t *testing.T) {
	ctx := context.Background()
	a, _ := forwardTestApp(t, nil)
	read := func() (state string, checks int, next int64) {
		if err := a.db.Reader.QueryRow(`SELECT state,checks,next_check_at FROM chat_access WHERE chat_id='dest:1'`).Scan(&state, &checks, &next); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := a.recordDestinationAccess(ctx, "dest:1", "a", "deleted", "CHANNEL_INVALID", false); err != nil {
		t.Fatal(err)
	}
	_, checks, first := read()
	if checks != 0 || first-time.Now().UnixMilli() > int64(66*time.Minute/time.Millisecond) {
		t.Fatalf("checks=%d next=%d", checks, first)
	}
	// A failure outside a re-check keeps the schedule.
	if err := a.recordDestinationAccess(ctx, "dest:1", "a", "deleted", "CHANNEL_INVALID", false); err != nil {
		t.Fatal(err)
	}
	if _, checks, next := read(); checks != 0 || next != first {
		t.Fatalf("checks=%d next=%d first=%d", checks, next, first)
	}
	if err := a.recordDestinationAccess(ctx, "dest:1", "a", "deleted", "CHANNEL_INVALID", true); err != nil {
		t.Fatal(err)
	}
	if state, checks, next := read(); state != "deleted" || checks != 1 || next-time.Now().UnixMilli() < int64(5*time.Hour/time.Millisecond) {
		t.Fatalf("state=%s checks=%d next=%d", state, checks, next)
	}
	if err := a.markDestinationReachable(ctx, "dest:1", "a"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM chat_access WHERE chat_id='dest:1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d %v", n, err)
	}
}

func TestDestinationKeyMatchesNode(t *testing.T) {
	for in, want := range map[string]string{"": "", "storage": "", "me": "", "saved": "", "-1001": "dest:-1001", "@Foo": "dest:@foo", "Bar": "dest:@bar"} {
		if got := destinationKey(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSelfDestructingMediaRecordsTTL(t *testing.T) {
	ctx := context.Background()
	a, fake := forwardTestApp(t, nil)
	fake.observing.Store(false)
	message := docMessage(5, 456, 9, "")
	message.Media.(*tg.MessageMediaDocument).SetTTLSeconds(15)
	if _, err := a.ingestTelegram(ctx, message, "", "Other", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	var ttl sql.NullInt64
	if err := a.db.Reader.QueryRow(`SELECT ttl_seconds FROM downloads WHERE group_id='-1000000000456' AND message_id=5`).Scan(&ttl); err != nil || ttl.Int64 != 15 {
		t.Fatalf("ttl=%v %v", ttl, err)
	}
	if _, err := a.ingestTelegram(ctx, docMessage(6, 456, 10, ""), "", "Other", "acct1", fiveBytes); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Reader.QueryRow(`SELECT ttl_seconds FROM downloads WHERE group_id='-1000000000456' AND message_id=6`).Scan(&ttl); err != nil || ttl.Valid {
		t.Fatalf("ordinary media got ttl=%v %v", ttl, err)
	}
}
