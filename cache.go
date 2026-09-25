package main

import (
	"encoding/json"
	"os"
	"sync"
)

// mtime has a resolution limit, so a file rewritten to the same length inside
// one tick reads as unchanged.
type key struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Inode   uint64 `json:"inode"`
}

type record struct {
	Path  string    `json:"path"`
	Key   key       `json:"key"`
	Entry fileEntry `json:"entry"`
}

type cache struct {
	path    string
	mu      sync.Mutex
	m       map[string]record
	enabled bool
	Hits    int
}

func loadCache(path string, enabled bool) *cache {
	c := &cache{path: path, m: map[string]record{}, enabled: enabled}
	if !enabled {
		return c
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var recs []record
	if json.Unmarshal(b, &recs) == nil {
		for _, r := range recs {
			c.m[r.Path] = r
		}
	}
	return c
}

func keyOf(info os.FileInfo) key {
	return key{info.Size(), info.ModTime().UnixNano(), inode(info)}
}

func (c *cache) get(path string, info os.FileInfo) (fileEntry, bool) {
	if !c.enabled {
		return fileEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.m[path]
	if !ok || r.Key != keyOf(info) {
		return fileEntry{}, false
	}
	c.Hits++
	return r.Entry, true
}

func (c *cache) put(path string, info os.FileInfo, e fileEntry) {
	if !c.enabled {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[path] = record{path, keyOf(info), e}
}

func (c *cache) save() error {
	if !c.enabled {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	recs := make([]record, 0, len(c.m))
	for _, r := range c.m {
		recs = append(recs, r)
	}
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, b, 0o644)
}
