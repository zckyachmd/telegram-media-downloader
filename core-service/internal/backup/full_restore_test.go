package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActivatePendingFullRestoreAcceptsMkdirTempStage(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	downloadsDir := filepath.Join(dataDir, "downloads")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "old.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	stageDir, err := os.MkdirTemp(root, ".tgdl-full-restore-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stageDir, "restored.txt"), []byte("restored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = PrepareFullRestore(dataDir, downloadsDir, stageDir); err != nil {
		t.Fatal(err)
	}

	activated, err := ActivatePendingFullRestore(dataDir, downloadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if !activated {
		t.Fatal("expected pending restore to activate")
	}
	if _, err = os.Stat(filepath.Join(dataDir, "restored.txt")); err != nil {
		t.Fatalf("restored stage was not activated: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dataDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old data directory was not replaced: %v", err)
	}
}

func TestValidRestoreStagePathMatchesMkdirTempNames(t *testing.T) {
	for _, name := range []string{
		".tgdl-full-restore-123456789",
		".tgdl-full-restore-1046589826",
	} {
		if !validRestoreStagePath(name) {
			t.Errorf("rejected MkdirTemp name %q", name)
		}
	}
	for _, name := range []string{
		".tgdl-full-restore-",
		".tgdl-full-restore-12345678901",
		".tgdl-full-restore-123456789_",
		"other-prefix-1234567890",
	} {
		if validRestoreStagePath(name) {
			t.Errorf("accepted invalid stage path %q", name)
		}
	}
}
