package client

import "github.com/zalando/go-keyring"

const (
	KeyringService = "kagi-cli"
	KeyringKey     = "session"
)

// LoadSession reads the saved session from the OS keyring (libsecret on
// Linux, Keychain on macOS, Credential Manager on Windows).
func LoadSession() (string, error) {
	return keyring.Get(KeyringService, KeyringKey)
}

func SaveSession(session string) error {
	return keyring.Set(KeyringService, KeyringKey, session)
}

func DeleteSession() error {
	return keyring.Delete(KeyringService, KeyringKey)
}
