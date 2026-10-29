package backup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const fullRestoreIntentName = ".tgdl-full-restore-pending.json"

type fullRestoreIntent struct {
	DataDir           string `json:"dataDir"`
	DownloadsDir      string `json:"downloadsDir"`
	StageDir          string `json:"stageDir"`
	DataRollback      string `json:"dataRollback"`
	DownloadsStage    string `json:"downloadsStage"`
	DownloadsRollback string `json:"downloadsRollback"`
}

func PrepareFullRestore(dataDir, downloadsDir, stageDir string) error {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	downloadsDir, err = filepath.Abs(downloadsDir)
	if err != nil {
		return err
	}
	stageDir, err = filepath.Abs(stageDir)
	if err != nil {
		return err
	}
	if filepath.Dir(stageDir) != filepath.Dir(dataDir) || !strings.HasPrefix(filepath.Base(stageDir), ".tgdl-full-restore-") {
		return errors.New("invalid full restore staging directory")
	}
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(suffix[:])
	intent := fullRestoreIntent{DataDir: dataDir, DownloadsDir: downloadsDir, StageDir: stageDir,
		DataRollback: dataDir + ".tgdl-restore-rollback-" + id}
	if externalDownloads(dataDir, downloadsDir) {
		intent.DownloadsStage = downloadsDir + ".tgdl-restore-stage-" + id
		intent.DownloadsRollback = downloadsDir + ".tgdl-restore-rollback-" + id
	}
	intentPath := filepath.Join(filepath.Dir(dataDir), fullRestoreIntentName)
	if _, err = os.Lstat(intentPath); err == nil {
		return errors.New("another full restore is already pending")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, name := range []string{intent.DataRollback, intent.DownloadsStage, intent.DownloadsRollback} {
		if name == "" {
			continue
		}
		if _, err = os.Lstat(name); err == nil {
			return errors.New("full restore rollback path already exists")
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if intent.DownloadsStage != "" {
		if err = prepareExternalRestore(intent); err != nil {
			return err
		}
	}
	published := false
	defer func() {
		if !published && intent.DownloadsStage != "" {
			_ = os.RemoveAll(intent.DownloadsStage)
		}
	}()
	temp, err := os.CreateTemp(filepath.Dir(dataDir), ".tgdl-full-restore-intent-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err == nil {
		err = json.NewEncoder(temp).Encode(intent)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Link(temp.Name(), intentPath); err != nil {
		return err
	}
	published = true
	return nil
}

func ActivatePendingFullRestore(dataDir, downloadsDir string) (bool, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return false, err
	}
	intentPath := filepath.Join(filepath.Dir(dataDir), fullRestoreIntentName)
	file, err := os.Open(intentPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var intent fullRestoreIntent
	decodeErr := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&intent)
	closeErr := file.Close()
	if err = errors.Join(decodeErr, closeErr); err != nil {
		return true, err
	}
	downloadsDir, err = filepath.Abs(downloadsDir)
	if err != nil {
		return true, err
	}
	if intent.DataDir != dataDir || intent.DownloadsDir != downloadsDir ||
		filepath.Dir(intent.StageDir) != filepath.Dir(dataDir) || !validRestoreStagePath(filepath.Base(intent.StageDir)) ||
		!validRestorePath(intent.DataRollback, dataDir+".tgdl-restore-rollback-") {
		return true, errors.New("full restore intent does not match configured data paths")
	}
	if intent.DownloadsStage != "" {
		if !externalDownloads(dataDir, downloadsDir) || filepath.Dir(intent.DownloadsStage) != filepath.Dir(downloadsDir) || filepath.Dir(intent.DownloadsRollback) != filepath.Dir(downloadsDir) ||
			!validRestorePath(intent.DownloadsStage, downloadsDir+".tgdl-restore-stage-") || !validRestorePath(intent.DownloadsRollback, downloadsDir+".tgdl-restore-rollback-") {
			return true, errors.New("full restore media intent does not match configured downloads path")
		}
		if _, stageErr := os.Lstat(intent.DownloadsStage); stageErr == nil {
			if err = os.Remove(filepath.Join(intent.DownloadsStage, ".tgdl-restore-ready")); err != nil {
				return true, err
			}
			if _, err = os.Lstat(intent.DownloadsRollback); errors.Is(err, fs.ErrNotExist) {
				if _, targetErr := os.Lstat(downloadsDir); targetErr == nil {
					err = os.Rename(downloadsDir, intent.DownloadsRollback)
					if err != nil {
						return true, err
					}
				} else if !errors.Is(targetErr, fs.ErrNotExist) {
					return true, targetErr
				}
			} else if err != nil {
				return true, err
			}
			if _, targetErr := os.Lstat(downloadsDir); errors.Is(targetErr, fs.ErrNotExist) {
				if err = os.Rename(intent.DownloadsStage, downloadsDir); err != nil {
					return true, err
				}
			} else if targetErr != nil {
				return true, targetErr
			}
		} else if !errors.Is(stageErr, fs.ErrNotExist) {
			return true, stageErr
		} else if _, targetErr := os.Lstat(downloadsDir); targetErr != nil {
			return true, targetErr
		}
	}
	if _, stageErr := os.Lstat(intent.StageDir); stageErr == nil {
		if _, rollbackErr := os.Lstat(intent.DataRollback); errors.Is(rollbackErr, fs.ErrNotExist) {
			if _, targetErr := os.Lstat(dataDir); targetErr == nil {
				if err = os.Rename(dataDir, intent.DataRollback); err != nil {
					return true, err
				}
			} else if !errors.Is(targetErr, fs.ErrNotExist) {
				return true, targetErr
			}
		} else if rollbackErr != nil {
			return true, rollbackErr
		}
		if err = preserveBackupDirectory(intent); err != nil {
			return true, err
		}
		if err = os.Rename(intent.StageDir, dataDir); err != nil {
			return true, err
		}
	} else if !errors.Is(stageErr, fs.ErrNotExist) {
		return true, stageErr
	} else if _, err = os.Lstat(dataDir); err != nil {
		return true, err
	}
	if err = os.Remove(intentPath); err != nil {
		return true, err
	}
	if intent.DownloadsRollback != "" {
		_ = os.RemoveAll(intent.DownloadsRollback)
	}
	_ = os.RemoveAll(intent.DataRollback)
	return true, nil
}

func validRestoreStagePath(value string) bool {
	const prefix = ".tgdl-full-restore-"
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > len(prefix)+10 {
		return false
	}
	for _, r := range value[len(prefix):] {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func validRestorePath(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+16 {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

func externalDownloads(dataDir, downloadsDir string) bool {
	rel, err := filepath.Rel(dataDir, downloadsDir)
	return err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func prepareExternalRestore(intent fullRestoreIntent) error {
	if _, err := os.Lstat(intent.DownloadsStage); err == nil {
		if _, readyErr := os.Stat(filepath.Join(intent.DownloadsStage, ".tgdl-restore-ready")); readyErr == nil {
			return nil
		}
		if err = os.RemoveAll(intent.DownloadsStage); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	source := filepath.Join(intent.StageDir, "downloads")
	if _, err := os.Lstat(source); errors.Is(err, fs.ErrNotExist) {
		return errors.New("full restore archive has no external downloads root")
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(intent.DownloadsDir), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(intent.DownloadsStage, 0700); err != nil {
		return err
	}
	if err := copyRestoreDirectory(source, intent.DownloadsStage); err != nil {
		_ = os.RemoveAll(intent.DownloadsStage)
		return err
	}
	if err := os.RemoveAll(source); err != nil {
		_ = os.RemoveAll(intent.DownloadsStage)
		return err
	}
	marker, err := os.OpenFile(filepath.Join(intent.DownloadsStage, ".tgdl-restore-ready"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return errors.Join(marker.Sync(), marker.Close())
}

func copyRestoreDirectory(source, destination string) error {
	src, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer dst.Close()
	return fs.WalkDir(src.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("full restore refuses media symlinks")
		}
		if entry.IsDir() {
			if name != "." {
				return dst.MkdirAll(name, 0700)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("full restore refuses non-regular media entries")
		}
		in, err := src.Open(name)
		if err != nil {
			return err
		}
		out, err := dst.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		return errors.Join(copyErr, in.Close(), out.Sync(), out.Close())
	})
}

func preserveBackupDirectory(intent fullRestoreIntent) error {
	old := filepath.Join(intent.DataRollback, "backups")
	new := filepath.Join(intent.StageDir, "backups")
	if _, err := os.Lstat(old); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(new); err == nil {
		return errors.New("restore staging unexpectedly contains backups")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(old, new)
}
