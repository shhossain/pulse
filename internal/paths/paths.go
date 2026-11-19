// Package paths locates Pulse's data directory and files.
package paths

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// BundleID is the macOS bundle identifier of Pulse.app.
const BundleID = "dev.pulse.menubar"

// Home returns the data directory. PULSE_HOME overrides it (used by tests).
func Home() string {
	if h := os.Getenv("PULSE_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Pulse")
	}
	return filepath.Join(home, ".local", "share", "pulse")
}

func DB() string         { return filepath.Join(Home(), "pulse.db") }
func ServerFile() string { return filepath.Join(Home(), "server.json") }
func PushToken() string  { return filepath.Join(Home(), "push.token") }
func AdminToken() string { return filepath.Join(Home(), "admin.token") }

// ServerInfo is written by the app so the CLI can find it.
type ServerInfo struct {
	URL     string `json:"url"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

func WriteServerInfo(si ServerInfo) error {
	b, _ := json.MarshalIndent(si, "", "  ")
	return writeFileAtomic(ServerFile(), b, 0o600)
}

func ReadServerInfo() (ServerInfo, error) {
	var si ServerInfo
	b, err := os.ReadFile(ServerFile())
	if err != nil {
		return si, err
	}
	err = json.Unmarshal(b, &si)
	return si, err
}

// ReadToken reads a token file ("" if missing).
func ReadToken(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// EnsureToken returns the token in path, creating a random one if missing.
func EnsureToken(path string) (string, error) {
	if t := ReadToken(path); t != "" {
		return t, nil
	}
	t := RandomHex(32)
	if err := writeFileAtomic(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// RotateToken replaces the token in path with a new random one.
func RotateToken(path string) (string, error) {
	t := RandomHex(32)
	if err := writeFileAtomic(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// AuditLog is where security-relevant events are recorded.
func AuditLog() string { return filepath.Join(Home(), "audit.log") }

// RandomHex returns n random bytes, hex encoded.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return os.Rename(tmp, path)
}
