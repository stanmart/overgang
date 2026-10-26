// Package state persists what must survive a restart: the identities apps have paired
// with, and the IDs handed out to devices, groups and moods. The app caches IDs, so
// they must not change when the config is reordered.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	path string

	mu   sync.Mutex
	data data
}

type data struct {
	// Identity → pre-shared key.
	Identities map[string]string `json:"identities"`
	// Namespace ("device", "group", "mood") → key → ID.
	IDs map[string]map[string]int `json:"ids"`
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, err
		}
	}
	if s.data.Identities == nil {
		s.data.Identities = map[string]string{}
	}
	if s.data.IDs == nil {
		s.data.IDs = map[string]map[string]int{}
	}
	return s, nil
}

// PSK returns the key paired with an identity.
func (s *Store) PSK(identity string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	psk, ok := s.data.Identities[identity]
	return psk, ok
}

// SetPSK records a newly paired identity.
func (s *Store) SetPSK(identity, psk string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Identities[identity] = psk
	return s.save()
}

// ID returns the ID for key in a namespace, assigning the next free one from base.
func (s *Store) ID(namespace, key string, base int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.data.IDs[namespace]
	if ids == nil {
		ids = map[string]int{}
		s.data.IDs[namespace] = ids
	}
	if id, ok := ids[key]; ok {
		return id, nil
	}
	next := base
	for _, id := range ids {
		next = max(next, id+1)
	}
	ids[key] = next
	return next, s.save()
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
