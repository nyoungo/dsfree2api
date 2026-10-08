// Package logfile writes logs to a daily file and compresses the previous
// day's file on rotation. It is an io.Writer meant to sit behind an slog
// handler, next to the stderr writer.
package logfile

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const dayLayout = "2006-01-02"

// Writer appends to path and rotates once per day: the retired file becomes
// "<base>-YYYY-MM-DD.log.gz" next to it.
type Writer struct {
	mu     sync.Mutex
	dir    string
	base   string
	path   string
	file   *os.File
	day    string
	closed bool
	now    func() time.Time
	wg     sync.WaitGroup
}

// New opens (or creates) path and immediately rotates a stale file left over
// from an earlier day.
func New(path string) (*Writer, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("log file path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &Writer{dir: dir, base: baseName(path), path: path, now: time.Now}
	if st, err := os.Stat(path); err == nil {
		day := st.ModTime().Format(dayLayout)
		if day != w.now().Format(dayLayout) && st.Size() > 0 {
			w.day = day
			if err := w.rotateLocked(); err != nil {
				return nil, err
			}
			return w, nil
		}
	}
	return w, w.openLocked()
}

// Write appends p, rotating first when the local day changed.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.file == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	if day := w.now().Format(dayLayout); day != w.day {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	return w.file.Write(p)
}

// Close flushes and waits for any in-flight compression.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var err error
	if w.file != nil {
		err = w.file.Close()
		w.file = nil
	}
	w.wg.Wait()
	return err
}

func (w *Writer) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.file = f
	w.day = w.now().Format(dayLayout)
	return nil
}

// rotateLocked retires the current file — renaming it first so the async
// compression can never read a freshly opened file — and starts a new one.
func (w *Writer) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	old, day := w.path, w.day
	if st, err := os.Stat(old); err == nil && st.Size() > 0 {
		tmp := old + ".rotating"
		if err := os.Rename(old, tmp); err == nil {
			gz := filepath.Join(w.dir, fmt.Sprintf("%s-%s.log.gz", w.base, day))
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				compressFile(tmp, gz)
			}()
		}
	} else {
		_ = os.Remove(old)
	}
	return w.openLocked()
}

// compressFile gzips src into dst and removes src on success; on any failure
// the raw .rotating file is left in place so no log data is ever lost.
// NOTE: the source handle is closed before removal — Windows refuses to
// delete a file that is still open.
func compressFile(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	out, err := os.Create(dst)
	if err != nil {
		_ = in.Close()
		return
	}
	gz := gzip.NewWriter(out)
	_, copyErr := io.Copy(gz, in)
	_ = in.Close()
	if copyErr != nil {
		gz.Close()
		out.Close()
		_ = os.Remove(dst)
		return
	}
	if err := gz.Close(); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return
	}
	_ = os.Remove(src)
}

func baseName(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}
