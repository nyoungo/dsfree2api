package logfile

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readGz(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", path, err)
	}
	defer gz.Close()
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestDailyRotationCompresses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dsfree2api.log")
	w, err := New(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	day1 := w.now()
	if _, err := w.Write([]byte("day-one\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Cross midnight: the next write rotates and compresses day one.
	w.now = func() time.Time { return day1.Add(24 * time.Hour) }
	if _, err := w.Write([]byte("day-two\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if !strings.Contains(string(cur), "day-two") || strings.Contains(string(cur), "day-one") {
		t.Fatalf("current log = %q", cur)
	}
	gzPath := filepath.Join(dir, "dsfree2api-"+day1.Format(dayLayout)+".log.gz")
	if got := readGz(t, gzPath); !strings.Contains(got, "day-one") {
		t.Fatalf("rotated log = %q", got)
	}
	if _, err := os.Stat(path + ".rotating"); !os.IsNotExist(err) {
		t.Error(".rotating file should be gone after compression")
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	w, err := New(filepath.Join(t.TempDir(), "x.log"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := w.Write([]byte("no\n")); err == nil {
		t.Fatal("write after close should fail")
	}
}

func TestStaleFileRotatedOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	w, err := New(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.Write([]byte("new\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	gzPath := filepath.Join(dir, "app-"+old.Format(dayLayout)+".log.gz")
	if got := readGz(t, gzPath); !strings.Contains(got, "old") {
		t.Fatalf("rotated = %q", got)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if !strings.Contains(string(cur), "new") {
		t.Fatalf("current = %q", cur)
	}
}
