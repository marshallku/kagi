package client

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Config holds non-secret CLI defaults (model, etc.). Persisted as JSON at
// $XDG_CONFIG_HOME/kagi/config.json (or $HOME/.config/kagi/config.json).
type Config struct {
	Model   string `json:"model,omitempty"`
	Profile string `json:"profile,omitempty"`
}

func ConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "kagi", "config.json")
}

// LoadConfig reads the config file. A missing file is not an error — first
// run users start with an empty Config.
func LoadConfig() (Config, error) {
	var cfg Config
	b, err := os.ReadFile(ConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func SaveConfig(cfg Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}
