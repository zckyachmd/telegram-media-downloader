package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var fullSnapshotPattern = regexp.MustCompile(`^full-\d{8}-\d{6}\.tgdf$`)

// CreateFullSnapshotWithPassword creates an encrypted full archive using a
// password-derived key. A fresh salt is stored in the archive header.
func (m *Manager) CreateFullSnapshotWithPassword(ctx context.Context, passphrase string) (Object, error) {
	m.op.Lock()
	defer m.op.Unlock()
	if err := m.available(); err != nil {
		return Object{}, err
	}
	if len(passphrase) < 12 {
		return Object{}, errors.New("backup password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Object{}, err
	}
	key, err := payloadKey(passphrase, salt)
	if err != nil {
		return Object{}, err
	}
	defer clear(key)
	folder := filepath.Join(m.opts.DataDir, "backups")
	if err = os.MkdirAll(folder, 0700); err != nil {
		return Object{}, err
	}
	var plainPath, encryptedPath string
	for stamp := time.Now(); ; stamp = stamp.Add(time.Second) {
		name := "full-" + stamp.Format("20060102-150405")
		plainPath = filepath.Join(folder, name+".tar.gz")
		encryptedPath = filepath.Join(folder, name+".tgdf")
		if _, err = os.Lstat(plainPath); errors.Is(err, fs.ErrNotExist) {
			if _, err = os.Lstat(encryptedPath); errors.Is(err, fs.ErrNotExist) {
				break
			}
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Object{}, err
		}
	}
	defer os.Remove(plainPath)
	_, _, err = m.prepareSnapshot(ctx, destination{}, job{Full: true, Snapshot: sql.NullString{String: plainPath, Valid: true}})
	if err != nil {
		return Object{}, err
	}
	source, err := os.Open(plainPath)
	if err != nil {
		return Object{}, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return Object{}, err
	}
	temp, err := os.CreateTemp(folder, ".full-encrypted-*")
	if err != nil {
		return Object{}, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = temp.Write([]byte("TGDF\x01")); err == nil {
		_, err = temp.Write(salt)
	}
	if err == nil {
		err = encryptPayload(ctx, temp, source, info.Size(), key)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Object{}, err
	}
	if err = os.Rename(tempPath, encryptedPath); err != nil {
		return Object{}, err
	}
	if err = os.Remove(plainPath); err != nil {
		_ = os.Remove(encryptedPath)
		return Object{}, fmt.Errorf("remove plaintext backup archive: %w", err)
	}
	info, err = os.Stat(encryptedPath)
	if err != nil {
		return Object{}, err
	}
	m.Log("info", fmt.Sprintf("full backup saved locally: %s", filepath.Base(encryptedPath)))
	return Object{Path: filepath.Base(encryptedPath), Size: info.Size(), Modified: info.ModTime()}, nil
}

// DecryptFullSnapshotWithPassword authenticates a locally stored or uploaded
// TGDF archive into a private plaintext file before returning it for parsing.
func (m *Manager) DecryptFullSnapshotWithPassword(ctx context.Context, archivePath, passphrase string) (string, error) {
	if len(passphrase) < 1 {
		return "", errors.New("backup password is required")
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("backup input must be a regular file")
	}
	source, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	var header [21]byte
	if _, err = io.ReadFull(source, header[:]); err != nil {
		return "", errors.New("invalid or truncated password-encrypted backup")
	}
	if string(header[:5]) != "TGDF\x01" {
		return "", errors.New("backup must use the password-encrypted TGDF format")
	}
	key, err := payloadKey(passphrase, header[5:])
	if err != nil {
		return "", err
	}
	defer clear(key)
	verified, err := os.CreateTemp(filepath.Join(m.opts.DataDir, "backups"), ".full-restore-verified-*.tar.gz")
	if err != nil {
		return "", err
	}
	verifiedPath := verified.Name()
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(verifiedPath)
		}
	}()
	if err = verified.Chmod(0600); err == nil {
		err = decryptPayload(ctx, verified, source, info.Size()-int64(len(header)), key)
	}
	if err == nil {
		err = verified.Sync()
	}
	if closeErr := verified.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("authenticate full backup: %w", err)
	}
	complete = true
	return verifiedPath, nil
}

func (m *Manager) FullSnapshots(context.Context) ([]Object, error) {
	folder := filepath.Join(m.opts.DataDir, "backups")
	entries, err := os.ReadDir(folder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	objects := make([]Object, 0, len(entries))
	for _, entry := range entries {
		if !fullSnapshotPattern.MatchString(entry.Name()) || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("full backup %q is not a regular file", entry.Name())
		}
		objects = append(objects, Object{Path: entry.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Modified.Equal(objects[j].Modified) {
			return objects[i].Path > objects[j].Path
		}
		return objects[i].Modified.After(objects[j].Modified)
	})
	return objects, nil
}

func (m *Manager) FullSnapshotPath(name string) (string, error) {
	if !fullSnapshotPattern.MatchString(name) {
		return "", errors.New("invalid full backup file name")
	}
	path := filepath.Join(m.opts.DataDir, "backups", name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("full backup is not a regular file")
	}
	return path, nil
}
