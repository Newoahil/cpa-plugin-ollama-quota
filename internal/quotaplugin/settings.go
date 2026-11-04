package quotaplugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var errSettings = errors.New("plugin settings could not be read or written; check state_dir and permissions")

type settingsStore struct {
	mu        sync.Mutex
	directory string
}

type savedSettings struct {
	APIKey string `json:"api_key"`
}

func (s *settingsStore) setDirectory(directory string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.directory = strings.TrimSpace(directory)
}

func defaultStateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base, err = os.UserHomeDir()
	}
	if err != nil || base == "" {
		return "", errSettings
	}
	return filepath.Join(base, "cpa-plugin-ollama-quota"), nil
}

func (s *settingsStore) path() (string, error) {
	directory := s.directory
	if directory == "" {
		var err error
		directory, err = defaultStateDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(directory, "settings.json"), nil
}

func (s *settingsStore) load() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path()
	if err != nil {
		return "", errSettings
	}
	directory := filepath.Dir(path)
	dirInfo, dirErr := os.Lstat(directory)
	if errors.Is(dirErr, os.ErrNotExist) { return "", nil }
	if dirErr != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 { return "", errSettings }
	if os.Chmod(directory, 0700) != nil { return "", errSettings }
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return "", errSettings
	}
	if err = os.Chmod(path, 0600); err != nil {
		return "", errSettings
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 16384 {
		return "", errSettings
	}
	var settings savedSettings
	if json.Unmarshal(raw, &settings) != nil {
		return "", errSettings
	}
	return strings.TrimSpace(settings.APIKey), nil
}

func (s *settingsStore) save(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path()
	if err != nil {
		return errSettings
	}
	directory := filepath.Dir(path)
	if os.MkdirAll(directory, 0700) != nil {
		return errSettings
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errSettings
	}
	if os.Chmod(directory, 0700) != nil {
		return errSettings
	}
	if info, err = os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errSettings
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errSettings
	}
	raw, err := json.Marshal(savedSettings{APIKey: key})
	if err != nil {
		return errSettings
	}
	// Write to a private file before replacing the live configuration, so a
	// failed write does not destroy the previously working key.
	file, err := os.CreateTemp(directory, ".settings-*")
	if err != nil {
		return errSettings
	}
	defer os.Remove(file.Name()) // Only our temporary file; renamed away on success.
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errSettings
	}
	if os.Rename(file.Name(), path) != nil {
		return errSettings
	}
	return nil
}

func maskedKey(key string) string {
	if key == "" {
		return ""
	}
	runes := []rune(key)
	// Very short credentials are fully masked; never return the whole key.
	if len(runes) <= 4 {
		return "••••"
	}
	return "••••" + string(runes[len(runes)-4:])
}
