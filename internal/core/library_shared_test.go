package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestLibrarySharedByTwoProcesses: two fragua servers on one ~/.pcb-library
// each kept their own index in memory and rewrote index.json through the same
// index.json.tmp. Concurrent imports failed with "rename …: no such file or
// directory", and a write from one server dropped every entry the other had
// added since it started.
func TestLibrarySharedByTwoProcesses(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for _, lib := range []struct {
		l      *Library
		prefix string
	}{{a, "a"}, {b, "b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				if _, err := lib.l.Put(LibraryEntry{Key: fmt.Sprintf("%s%02d", lib.prefix, i)}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	fresh, err := OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.Count(); got != 2*n {
		t.Fatalf("index.json holds %d entries, want %d (lost updates)", got, 2*n)
	}
	// Each instance sees what the other one wrote.
	if _, ok := a.Get("b07"); !ok {
		t.Fatal("library a cannot see an entry library b added")
	}
	if _, err := os.Stat(filepath.Join(root, "index.json.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock file left behind: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// A lock file left by a crashed process must not block the library forever.
func TestLibraryStaleLockIsTaken(t *testing.T) {
	root := t.TempDir()
	lib, err := OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "index.json.lock")
	if err := os.WriteFile(lock, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * libraryLockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Put(LibraryEntry{Key: "x"}); err != nil {
		t.Fatal(err)
	}
}
