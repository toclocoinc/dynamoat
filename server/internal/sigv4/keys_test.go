package sigv4

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// bumpMtime moves path's modification time forward by a minute so a reload
// detects the change regardless of the filesystem's timestamp granularity.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	when := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func writeKeyFile(t *testing.T, dir, name, secret string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKeyStoreReloadPicksUpAddedAndRemoved(t *testing.T) {
	dir := t.TempDir()
	writeKeyFile(t, dir, "AKID1", "secret1")
	writeKeyFile(t, dir, "AKID2", "secret2")

	ks, err := NewKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ks.Secret("AKID1"); !ok || got != "secret1" {
		t.Fatalf("Secret(AKID1) = %q, %v; want secret1, true", got, ok)
	}

	// Add AKID3, remove AKID2.
	writeKeyFile(t, dir, "AKID3", "secret3")
	if err := os.Remove(filepath.Join(dir, "AKID2")); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, dir)

	added, removed, err := ks.ReloadIfChanged()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"AKID3"}) {
		t.Fatalf("added = %v, want [AKID3]", added)
	}
	if !reflect.DeepEqual(removed, []string{"AKID2"}) {
		t.Fatalf("removed = %v, want [AKID2]", removed)
	}
	if got, ok := ks.Secret("AKID3"); !ok || got != "secret3" {
		t.Fatalf("Secret(AKID3) = %q, %v; want secret3, true", got, ok)
	}
	if _, ok := ks.Secret("AKID2"); ok {
		t.Fatal("Secret(AKID2) still resolves after removal")
	}
	if ks.Len() != 2 {
		t.Fatalf("Len = %d, want 2", ks.Len())
	}

	// Nothing changed since: the next check reloads nothing.
	added, removed, err = ks.ReloadIfChanged()
	if err != nil || added != nil || removed != nil {
		t.Fatalf("second ReloadIfChanged = %v, %v, %v; want nil, nil, nil", added, removed, err)
	}
}

func TestKeyStoreBrokenFileKeepsOldKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys")
	if err := os.WriteFile(p, []byte("AKID1 secret1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ks, err := NewKeyStore(p)
	if err != nil {
		t.Fatal(err)
	}

	// Break the file (a malformed line), bump its mtime, and reload.
	if err := os.WriteFile(p, []byte("justoneword\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, p)

	if _, _, err := ks.ReloadIfChanged(); err == nil {
		t.Fatal("ReloadIfChanged succeeded on a broken file, want an error")
	}
	if got, ok := ks.Secret("AKID1"); !ok || got != "secret1" {
		t.Fatalf("Secret(AKID1) = %q, %v after failed reload; want secret1, true", got, ok)
	}
	if ks.Len() != 1 {
		t.Fatalf("Len = %d after failed reload, want 1", ks.Len())
	}
}

func TestKeyStoreEmptyDirectoryKeepsOldKeys(t *testing.T) {
	dir := t.TempDir()
	writeKeyFile(t, dir, "AKID1", "secret1")

	ks, err := NewKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Removing every key file makes the reload fail; the old keys must survive.
	if err := os.Remove(filepath.Join(dir, "AKID1")); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, dir)

	if _, _, err := ks.ReloadIfChanged(); err == nil {
		t.Fatal("ReloadIfChanged succeeded on an empty directory, want an error")
	}
	if got, ok := ks.Secret("AKID1"); !ok || got != "secret1" {
		t.Fatalf("Secret(AKID1) = %q, %v after failed reload; want secret1, true", got, ok)
	}
}

func TestKeyStoreReloadFileLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "keys")
	if err := os.WriteFile(p, []byte("AKID1 secret1\nAKID2 secret2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ks, err := NewKeyStore(p)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte("AKID1 secret1\nAKID3 secret3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, p)

	added, removed, err := ks.ReloadIfChanged()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"AKID3"}) || !reflect.DeepEqual(removed, []string{"AKID2"}) {
		t.Fatalf("added = %v, removed = %v; want [AKID3], [AKID2]", added, removed)
	}
}
