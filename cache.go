package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// cache is what the proxy learned from the backend last time it ran: the
// initialize result (capabilities, server info, instructions) and every
// list the backend declared. Tool schemas, no data, no secrets.
type cache struct {
	Version               int                        `json:"version"`
	ClientProtocolVersion string                     `json:"clientProtocolVersion"`
	Initialize            json.RawMessage            `json:"initialize"`
	Lists                 map[string]json.RawMessage `json:"lists"`
	SavedAt               time.Time                  `json:"savedAt"`
}

const cacheVersion = 1

// declares reports whether the cached initialize result carries a capability.
func (c *cache) declares(capability string) bool {
	var r struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(c.Initialize, &r); err != nil {
		return false
	}
	_, ok := r.Capabilities[capability]
	return ok
}

// clone returns a copy safe to modify while readers hold the original.
func (c *cache) clone() *cache {
	out := *c
	out.Lists = make(map[string]json.RawMessage, len(c.Lists))
	for k, v := range c.Lists {
		out.Lists[k] = v
	}
	return &out
}

// loadCache reads the cache at path; a missing or unreadable file is no cache.
func loadCache(path string) *cache {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c cache
	if json.Unmarshal(b, &c) != nil || c.Version != cacheVersion || len(c.Initialize) == 0 {
		return nil
	}
	if c.Lists == nil {
		c.Lists = map[string]json.RawMessage{}
	}
	return &c
}

// saveCache writes the cache atomically: a temp file in the same
// directory, then a rename, so a reader never sees a half-written file.
func saveCache(path string, c *cache) error {
	stamped := *c
	stamped.Version = cacheVersion
	stamped.SavedAt = time.Now().UTC()
	b, err := json.MarshalIndent(stamped, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// cacheDir is $XDG_CACHE_HOME/mcp-defer, else the OS cache directory.
func cacheDir() (string, error) {
	if base := os.Getenv("XDG_CACHE_HOME"); base != "" {
		return filepath.Join(base, "mcp-defer"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "mcp-defer"), nil
}

// withListChanged marks the tools, prompts and resources capabilities
// as sending list_changed: the proxy sends them itself, once the real
// list turns out to differ from the cache.
func withListChanged(initResult json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(initResult))
	dec.UseNumber()
	var r map[string]any
	if dec.Decode(&r) != nil {
		return initResult
	}
	caps, _ := r["capabilities"].(map[string]any)
	for _, k := range []string{"tools", "prompts", "resources"} {
		if v, ok := caps[k].(map[string]any); ok {
			v["listChanged"] = true
		}
	}
	out, err := json.Marshal(r)
	if err != nil {
		return initResult
	}
	return out
}
