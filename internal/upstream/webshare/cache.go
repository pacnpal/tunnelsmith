package webshare

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Cache persists the proxy list to disk so the binary can keep serving
// when Webshare's API is unreachable at startup or during a refresh
// tick. Shape mirrors mullvad.Cache: tmp-and-rename writes, opaque
// readback, no schema versioning (Proxy's JSON tags are the contract).
type Cache struct {
	Path string
}

// ErrNoCache is returned when Read is called against an empty cache.
var ErrNoCache = errors.New("webshare: cache empty")

// Read returns the cached proxy list. Returns ErrNoCache when the cache
// has never been populated.
func (c *Cache) Read() ([]Proxy, error) {
	if c == nil || c.Path == "" {
		return nil, ErrNoCache
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoCache
		}
		return nil, err
	}
	var out []Proxy
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("webshare: decode cached list: %w", err)
	}
	return out, nil
}

// Write atomically persists the given proxy list via tmp-and-rename.
func (c *Cache) Write(list []Proxy) error {
	if c == nil || c.Path == "" {
		return ErrNoCache
	}
	dir := filepath.Dir(c.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(c.Path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, c.Path)
}
