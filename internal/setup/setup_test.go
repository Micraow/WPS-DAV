package setup

import (
	"os"
	"path/filepath"
	"testing"

	"wpsdav/internal/config"
)

func TestInitAndAgentKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wpsdav.json")
	cfg, pass, err := Init(path, false, false, "127.0.0.1:8705", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pass) != 20 || cfg.Upload.Backend != config.UploadPrivate {
		t.Fatalf("pass=%q cfg=%+v", pass, cfg.Upload)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	key := "apik:test.secret"
	if err := SaveAgentKey(cfg, key); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Upload.Backend != config.UploadPrivate || !loaded.HasAgentKey() {
		t.Fatalf("backend=%q key=%v", loaded.Upload.Backend, loaded.HasAgentKey())
	}
	if loaded.AgentKey() != key {
		t.Fatalf("key mismatch")
	}
}
