package client

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LastSession is the most recently used thread, persisted so a CLI user can
// continue it with `kagi chat --resume "..."`. Stored at
// $XDG_STATE_HOME/kagi/last-session.json (state dir, not config —
// regenerable / ephemeral).
type LastSession struct {
	ThreadID  string    `json:"thread_id"`
	MessageID string    `json:"message_id"`
	Title     string    `json:"title,omitempty"`
	Model     string    `json:"model,omitempty"`
	Profile   string    `json:"profile,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func StatePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(base, "kagi", "last-session.json")
}

func LoadLastSession() (LastSession, error) {
	var s LastSession
	b, err := os.ReadFile(StatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
}

func SaveLastSession(s LastSession) error {
	p := StatePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}
