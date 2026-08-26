package config

import "testing"

func TestSaveCanReplaceExistingConfig(t *testing.T) {
	arkDir := t.TempDir()
	cfg := &Config{
		Version:          1,
		RepositoryID:     "repo-id",
		DefaultActorID:   "actor-id",
		DefaultActorType: "human",
		Remote:           "https://one.example.com",
	}
	if err := Save(arkDir, cfg); err != nil {
		t.Fatalf("first save: %v", err)
	}

	cfg.Remote = "https://two.example.com"
	if err := Save(arkDir, cfg); err != nil {
		t.Fatalf("replacement save: %v", err)
	}

	got, err := Load(arkDir)
	if err != nil {
		t.Fatalf("load replacement: %v", err)
	}
	if got.Remote != cfg.Remote {
		t.Fatalf("remote = %q, want %q", got.Remote, cfg.Remote)
	}
}
