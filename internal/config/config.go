// Package config reads and writes .ark/config.toml.
package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/elk-work/ark/internal/records"
)

// Config is the repository-level configuration stored in .ark/config.toml.
// Credentials never belong here; see docs/v1-spec.md §20.
type Config struct {
	Version          int    `toml:"version"`
	RepositoryID     string `toml:"repository_id"`
	Remote           string `toml:"remote,omitempty"`
	DefaultActorID   string `toml:"default_actor_id"`
	DefaultActorType string `toml:"default_actor_type"`
	// RequireElkParent makes `ark task create` refuse a task with no `--elk`
	// parent. A client-side rule for a repository bound to an Elk workspace,
	// switched on only after that workspace's backfill has given every open
	// task a parent; the sync service never enforces it.
	RequireElkParent bool `toml:"require_elk_parent,omitempty"`
}

func Load(arkDir string) (*Config, error) {
	path := filepath.Join(arkDir, "config.toml")
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		if os.IsNotExist(err) {
			return nil, records.NotFoundf("no Ark configuration at %s (run `ark init`)", path)
		}
		return nil, &records.Error{Kind: records.KindGeneral, Message: "parse " + path, Err: err}
	}
	return &c, nil
}

func Save(arkDir string, c *Config) error {
	path := filepath.Join(arkDir, "config.toml")
	f, err := os.CreateTemp(arkDir, "config-*.toml")
	if err != nil {
		return &records.Error{Kind: records.KindGeneral, Message: "write config", Err: err}
	}
	tmp := f.Name()
	enc := toml.NewEncoder(f)
	if err := enc.Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return &records.Error{Kind: records.KindGeneral, Message: "encode config", Err: err}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return &records.Error{Kind: records.KindGeneral, Message: "write config", Err: err}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return &records.Error{Kind: records.KindGeneral, Message: "write config", Err: err}
	}
	return nil
}
