package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 2026-09-11 L118: every concurrent turn materialises the same config.json, and
// writeWorkerFile used ONE shared "<path>.tmp" — so the second writer's
// os.Remove unlinked the first's inode mid-write, its O_EXCL open then
// succeeded on a name the first still had open, and the renames raced. A turn
// could read a truncated, half-written or missing config and fall back to
// defaults with nothing said.
func TestWriteWorkerFileIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	payloads := [][]byte{
		[]byte(strings.Repeat("a", 64<<10)),
		[]byte(strings.Repeat("b", 64<<10)),
		[]byte(strings.Repeat("c", 64<<10)),
	}
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			writeWorkerFile(path, payloads[i%len(payloads)])
		}(i)
	}
	// Readers see either nothing or ONE writer's complete bytes, never a mix.
	stop := make(chan struct{})
	bad := make(chan string, 1)
	var rwg sync.WaitGroup
	for i := 0; i < 4; i++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				ok := false
				for _, p := range payloads {
					if bytes.Equal(b, p) {
						ok = true
					}
				}
				if !ok {
					select {
					case bad <- fmt.Sprintf("read %d bytes that match no writer's payload", len(b)):
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	rwg.Wait()
	select {
	case msg := <-bad:
		t.Fatal(msg)
	default:
	}
	// A direct consequence, and the one that is deterministic: another
	// writer's temporary file is never destroyed. The old shared name was
	// unlinked by whoever came second — mid-write, out from under the first.
	sibling := path + ".tmp"
	if err := os.WriteFile(sibling, []byte("another writer's work in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWorkerFile(path, payloads[0])
	if b, err := os.ReadFile(sibling); err != nil || string(b) != "another writer's work in progress" {
		t.Fatalf("a concurrent writer's temporary file was destroyed: %v %q", err, b)
	}
	_ = os.Remove(sibling)

	// And no temporary files are left behind.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "config.json" {
			t.Errorf("leftover temporary file %q", e.Name())
		}
	}
}
