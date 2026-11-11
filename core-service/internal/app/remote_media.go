package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

var driveMediaCacheMu sync.Mutex
var driveMediaSeen = map[string]uint8{}

const (
	driveMediaBlockSize  = int64(1 << 20)
	driveMediaCacheLimit = int64(32 << 20)
)

type mediaByteRange struct{ start, end int64 }

type driveMediaMetrics struct {
	bytesServed       atomic.Int64
	cacheHits         atomic.Int64
	cacheMisses       atomic.Int64
	cacheWrites       atomic.Int64
	cacheEvictions    atomic.Int64
	firstByteCount    atomic.Int64
	firstByteNanos    atomic.Int64
	firstByteMaxNanos atomic.Int64
	firstByteBuckets  [9]atomic.Int64
}

func (m *driveMediaMetrics) snapshot() map[string]int64 {
	count := m.firstByteCount.Load()
	avg := int64(0)
	if count > 0 {
		avg = m.firstByteNanos.Load() / count
	}
	bounds := [...]int64{50e6, 100e6, 250e6, 500e6, 1e9, 2e9, 5e9, 10e9}
	percentile := func(p int64) int64 {
		rank := (count*p + 99) / 100
		if rank == 0 {
			return 0
		}
		var cumulative int64
		for i := range m.firstByteBuckets {
			cumulative += m.firstByteBuckets[i].Load()
			if cumulative >= rank {
				if i == len(bounds) {
					return m.firstByteMaxNanos.Load()
				}
				return bounds[i]
			}
		}
		return m.firstByteMaxNanos.Load()
	}
	return map[string]int64{
		"bytesServed": m.bytesServed.Load(), "cacheHits": m.cacheHits.Load(),
		"cacheMisses": m.cacheMisses.Load(), "cacheWrites": m.cacheWrites.Load(),
		"cacheEvictions": m.cacheEvictions.Load(), "firstByteSamples": count,
		"averageFirstByteNanos": avg, "maxFirstByteNanos": m.firstByteMaxNanos.Load(),
		"p50FirstByteNanos": percentile(50), "p95FirstByteNanos": percentile(95),
	}
}

func (m *driveMediaMetrics) recordFirstByte(d time.Duration) {
	nanos := d.Nanoseconds()
	m.firstByteCount.Add(1)
	m.firstByteNanos.Add(nanos)
	bounds := [...]int64{50e6, 100e6, 250e6, 500e6, 1e9, 2e9, 5e9, 10e9}
	bucket := len(bounds)
	for i, bound := range bounds {
		if nanos <= bound {
			bucket = i
			break
		}
	}
	m.firstByteBuckets[bucket].Add(1)
	for max := m.firstByteMaxNanos.Load(); nanos > max; max = m.firstByteMaxNanos.Load() {
		if m.firstByteMaxNanos.CompareAndSwap(max, nanos) {
			break
		}
	}
}

func parseMediaRange(value string, size int64) (mediaByteRange, bool) {
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size <= 0 {
		return mediaByteRange{}, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return mediaByteRange{}, false
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return mediaByteRange{}, false
		}
		return mediaByteRange{max(0, size-suffix), size - 1}, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return mediaByteRange{}, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return mediaByteRange{}, false
		}
		end = min(end, size-1)
	}
	return mediaByteRange{start, end}, true
}

func (a *App) serveDriveBackedMedia(w http.ResponseWriter, r *http.Request, destinationID int64, remotePath, fileName string, size int64, fileHash string) {
	started := time.Now()
	firstByteRecorded := false
	if r.Method != http.MethodHead {
		select {
		case a.remoteStreamSlots <- struct{}{}:
			defer func() { <-a.remoteStreamSlots }()
		case <-r.Context().Done():
			return
		}
	}
	if size <= 0 {
		writeText(w, r, http.StatusBadGateway, "Remote media size is unavailable")
		return
	}
	etag := `"drive-` + strconv.FormatInt(destinationID, 10) + `-` + strconv.FormatInt(size, 10) + `-` + fileHash + `"`
	selected := mediaByteRange{0, size - 1}
	partial := r.Header.Get("Range") != ""
	if partial && r.Header.Get("If-Range") != "" && r.Header.Get("If-Range") != etag {
		partial = false
	}
	if partial {
		var ok bool
		selected, ok = parseMediaRange(r.Header.Get("Range"), size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	firstBlock := selected.start / driveMediaBlockSize * driveMediaBlockSize
	var firstData []byte
	if r.Method != http.MethodHead {
		var err error
		firstData, err = a.driveMediaBlock(r, destinationID, remotePath, fileHash, firstBlock, size)
		if err != nil {
			if r.Context().Err() == nil {
				a.logf("Google Drive media read failed for destination #%d: %v", destinationID, err)
			}
			writeText(w, r, http.StatusBadGateway, "Google Drive media is temporarily unavailable")
			return
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(fileName)))
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	disposition := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", disposition+"; filename=\""+asciiFileName(fileName)+"\"; filename*=UTF-8''"+encodeFileComponent(fileName))
	w.Header().Set("ETag", etag)
	length := selected.end - selected.start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", selected.start, selected.end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	for offset := selected.start; offset <= selected.end; {
		blockStart := offset / driveMediaBlockSize * driveMediaBlockSize
		block := firstData
		if blockStart != firstBlock || block == nil {
			var err error
			block, err = a.driveMediaBlock(r, destinationID, remotePath, fileHash, blockStart, size)
			if err != nil {
				if r.Context().Err() == nil {
					a.logf("Google Drive media read failed for destination #%d: %v", destinationID, err)
				}
				return
			}
		}
		firstData = nil
		from := offset - blockStart
		to := min(int64(len(block)), selected.end-blockStart+1)
		if to <= from {
			return
		}
		chunk := block[from:to]
		n, err := w.Write(chunk)
		if n > 0 {
			a.driveMediaMetrics.bytesServed.Add(int64(n))
			if !firstByteRecorded {
				a.driveMediaMetrics.recordFirstByte(time.Since(started))
				firstByteRecorded = true
			}
		}
		if err != nil || n != len(chunk) {
			return
		}
		offset = blockStart + to
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (a *App) driveMediaBlock(r *http.Request, destinationID int64, remotePath, fileHash string, start, size int64) ([]byte, error) {
	if r.Context().Err() != nil {
		return nil, r.Context().Err()
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%d", destinationID, remotePath, fileHash, start)))
	cacheDir := filepath.Join(a.dataDir, "cache", "gdrive-range")
	name := hex.EncodeToString(key[:])
	cachePath := filepath.Join(cacheDir, name)
	admissionKey := cachePath
	want := min(driveMediaBlockSize, size-start)
	cacheable := size > driveMediaCacheLimit && fileHash != ""
	if cacheable {
		if data, err := os.ReadFile(cachePath); err == nil && int64(len(data)) == want {
			a.driveMediaMetrics.cacheHits.Add(1)
			_ = os.Chtimes(cachePath, time.Now(), time.Now())
			return data, nil
		}
		a.driveMediaMetrics.cacheMisses.Add(1)
	}
	result := a.remoteBlocks.DoChan(name, func() (any, error) {
		if cacheable {
			if data, err := os.ReadFile(cachePath); err == nil && int64(len(data)) == want {
				a.driveMediaMetrics.cacheHits.Add(1)
				_ = os.Chtimes(cachePath, time.Now(), time.Now())
				return data, nil
			}
		}
		data, total, err := a.backups.MediaRange(r.Context(), destinationID, remotePath, start, want)
		if err != nil {
			return nil, err
		}
		if total != size || int64(len(data)) != want {
			return nil, errors.New("Google Drive object size changed")
		}
		return data, nil
	})
	var value singleflight.Result
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case value = <-result:
	}
	if value.Err != nil {
		return nil, value.Err
	}
	data, ok := value.Val.([]byte)
	if !ok {
		return nil, errors.New("Google Drive range response is invalid")
	}
	if !cacheable {
		return data, nil
	}
	if value.Shared {
		return data, nil
	}
	driveMediaCacheMu.Lock()
	seen := driveMediaSeen[admissionKey]
	if seen == 0 && len(driveMediaSeen) >= 512 {
		driveMediaSeen = map[string]uint8{}
	}
	if seen < 2 {
		driveMediaSeen[admissionKey] = seen + 1
	}
	driveMediaCacheMu.Unlock()
	if seen == 0 {
		return data, nil
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return data, nil
	}
	driveMediaCacheMu.Lock()
	defer driveMediaCacheMu.Unlock()
	tmp, err := os.CreateTemp(cacheDir, ".range-*.tmp")
	if err != nil {
		return data, nil
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp.Name())
		return data, nil
	}
	if err = os.Rename(tmp.Name(), cachePath); err != nil {
		_ = os.Remove(tmp.Name())
	} else {
		a.driveMediaMetrics.cacheWrites.Add(1)
		a.driveMediaMetrics.cacheEvictions.Add(a.pruneDriveMediaCache(cacheDir))
	}
	return data, nil
}

func (a *App) pruneDriveMediaCache(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type cached struct {
		path     string
		size     int64
		modified time.Time
	}
	files := make([]cached, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".range-") {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, cached{filepath.Join(dir, entry.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= driveMediaCacheLimit {
		return 0
	}
	var evicted int64
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	for _, file := range files {
		if total <= driveMediaCacheLimit {
			break
		}
		if os.Remove(file.path) == nil {
			total -= file.size
			evicted++
		}
	}
	return evicted
}
