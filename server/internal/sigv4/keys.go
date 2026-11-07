package sigv4

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Keys looks up the secret for an access key ID.
type Keys interface {
	Secret(accessKeyID string) (string, bool)
}

// StaticKeys is an in-memory Keys.
type StaticKeys map[string]string

func (k StaticKeys) Secret(id string) (string, bool) {
	s, ok := k[id]
	return s, ok
}

// LoadKeys reads access keys from a file ("<id> <secret>" per line, with "#"
// comments) or, when path is a directory, from one file per key (name = ID,
// trimmed content = secret), as a mounted Kubernetes Secret appears.
func LoadKeys(path string) (StaticKeys, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	keys := StaticKeys{}
	if st.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			// Stat follows symlinks, which Kubernetes uses for secret files.
			fi, err := os.Stat(filepath.Join(path, e.Name()))
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(path, e.Name()))
			if err != nil {
				return nil, err
			}
			keys[e.Name()] = strings.TrimSpace(string(b))
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		n := 0
		for sc.Scan() {
			n++
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 2 {
				return nil, fmt.Errorf("%s:%d: expected \"<access key id> <secret>\"", path, n)
			}
			keys[fields[0]] = fields[1]
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no access keys found in " + path)
	}
	return keys, nil
}

// KeyStore holds the access keys loaded from a path and swaps a new set in
// atomically on reload. Secret reads a snapshot, so an in-flight request sees
// either the whole old set or the whole new set, never a mix.
type KeyStore struct {
	path string

	keys atomic.Pointer[StaticKeys]

	mu  sync.Mutex
	mod time.Time
}

// NewKeyStore loads the access keys at path.
func NewKeyStore(path string) (*KeyStore, error) {
	k := &KeyStore{path: path}
	if _, _, err := k.reloadLocked(); err != nil {
		return nil, err
	}
	mod, err := keysModTime(path)
	if err != nil {
		return nil, err
	}
	k.mod = mod
	return k, nil
}

// Secret returns the secret for id, or false when id is not loaded.
func (k *KeyStore) Secret(id string) (string, bool) {
	keys := k.keys.Load()
	if keys == nil {
		return "", false
	}
	s, ok := (*keys)[id]
	return s, ok
}

// Len returns the number of keys currently loaded.
func (k *KeyStore) Len() int {
	keys := k.keys.Load()
	if keys == nil {
		return 0
	}
	return len(*keys)
}

// Reload re-reads the key file(s) and swaps the new set in, returning the key
// IDs added and removed. On error the current keys are kept.
func (k *KeyStore) Reload() (added, removed []string, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	mod, err := keysModTime(k.path)
	if err != nil {
		return nil, nil, err
	}
	added, removed, err = k.reloadLocked()
	if err != nil {
		return nil, nil, err
	}
	k.mod = mod
	return added, removed, nil
}

// ReloadIfChanged re-reads the key file(s) when their modification time has
// moved since the last successful load; otherwise it does nothing and returns
// nil, nil, nil. On error the current keys are kept.
func (k *KeyStore) ReloadIfChanged() (added, removed []string, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	mod, err := keysModTime(k.path)
	if err != nil {
		return nil, nil, err
	}
	if !mod.After(k.mod) {
		return nil, nil, nil
	}
	added, removed, err = k.reloadLocked()
	if err != nil {
		return nil, nil, err
	}
	k.mod = mod
	return added, removed, nil
}

// reloadLocked loads the current keys and swaps them in, returning the key IDs
// that appeared and disappeared. The caller holds k.mu.
func (k *KeyStore) reloadLocked() (added, removed []string, err error) {
	next, err := LoadKeys(k.path)
	if err != nil {
		return nil, nil, err
	}
	cur := k.keys.Load()
	if cur != nil {
		for id := range *cur {
			if _, ok := next[id]; !ok {
				removed = append(removed, id)
			}
		}
		for id := range next {
			if _, ok := (*cur)[id]; !ok {
				added = append(added, id)
			}
		}
	} else {
		for id := range next {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	k.keys.Store(&next)
	return added, removed, nil
}

// keysModTime is the modification fingerprint of the key file(s): the file's
// mtime, or for a directory the latest mtime of the directory and every
// regular file in it (a Kubernetes Secret mount updates files in place and
// repoints symlinks). It mirrors the set of files LoadKeys reads.
func keysModTime(path string) (time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	latest := st.ModTime()
	if !st.IsDir() {
		return latest, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return time.Time{}, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := os.Stat(filepath.Join(path, e.Name()))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest, nil
}
