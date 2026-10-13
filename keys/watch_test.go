package keys

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// record is one line of the log, with its attributes as text.
type record struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// capture is a slog.Handler that keeps what is logged.
type capture struct {
	mu      sync.Mutex
	records []record
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capture) WithGroup(string) slog.Handler            { return c }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	rec := record{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	return nil
}

// at returns the records of level.
func (c *capture) at(level slog.Level) []record {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []record
	for _, r := range c.records {
		if r.level == level {
			out = append(out, r)
		}
	}
	return out
}

func newLog() (*slog.Logger, *capture) {
	c := &capture{}
	return slog.New(c), c
}

// writeFile replaces the file at path with content at once, as a
// ConfigMap's update does, so that Watch never reads half of it.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// waitFor waits up to 2 seconds for cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 2s for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitReads waits until Watch has read the file twice more, so that at
// least one whole reading began after what the test did last.
func waitReads(t *testing.T, l *List) {
	t.Helper()
	start := l.reads.Load()
	waitFor(t, "two more readings", func() bool { return l.reads.Load() >= start+2 })
}

// watch runs l.Watch with an interval of 5 ms until the test ends.
func watch(t *testing.T, l *List) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Watch(ctx, 5*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func countChanges(l *List) *atomic.Int32 {
	var n atomic.Int32
	l.OnChange(func() { n.Add(1) })
	return &n
}

func TestOpenMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	log, _ := newLog()
	l, err := Open(path, log)
	if err == nil {
		t.Fatalf("no error, list %v", l)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name %s", err, path)
	}
}

func TestOpenLogsRejectedLinesAndCount(t *testing.T) {
	key, line := newKey(t)
	_, other := newKey(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	writeFile(t, path, line+" me\nno-pty "+other+"\n# fine\nnot a key\n")
	log, c := newLog()
	l, err := Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Lookup(key); !ok {
		t.Error("the good key is not found")
	}
	warns := c.at(slog.LevelWarn)
	if len(warns) != 2 {
		t.Fatalf("warnings = %+v, want two", warns)
	}
	for i, want := range []string{"2", "4"} {
		if warns[i].attrs["line"] != want || warns[i].attrs["reason"] == "" {
			t.Errorf("warning %d = %+v, want line %s with a reason", i, warns[i], want)
		}
	}
	infos := c.at(slog.LevelInfo)
	if len(infos) != 1 || infos[0].attrs["keys"] != "1" || infos[0].attrs["rejected"] != "2" {
		t.Errorf("info = %+v, want one line with keys 1, rejected 2", infos)
	}
}

func TestReloadCountsBothKinds(t *testing.T) {
	_, a := newKey(t)
	_, b := newKey(t)
	_, c := newKey(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	writeFile(t, path, a+" a\n"+b+" b\nread-only "+c+" c\n")
	log, rec := newLog()
	if _, err := Open(path, log); err != nil {
		t.Fatal(err)
	}
	infos := rec.at(slog.LevelInfo)
	if len(infos) != 1 {
		t.Fatalf("info = %+v, want one line", infos)
	}
	for k, want := range map[string]string{"keys": "3", "full": "2", "read_only": "1"} {
		if got := infos[0].attrs[k]; got != want {
			t.Errorf("%s = %q, want %s", k, got, want)
		}
	}
}

func TestOpenEmptyFileWarns(t *testing.T) {
	for name, content := range map[string]string{
		"empty":              "",
		"only comments":      "# nobody yet\n\n",
		"only rejected keys": "no-pty ssh-ed25519 AAAA\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "authorized_keys")
			writeFile(t, path, content)
			log, c := newLog()
			if _, err := Open(path, log); err != nil {
				t.Fatalf("error %v", err)
			}
			found := false
			for _, r := range c.at(slog.LevelWarn) {
				found = found || strings.Contains(r.msg, "no key")
			}
			if !found {
				t.Errorf("no warning of no key in %+v", c.records)
			}
			infos := c.at(slog.LevelInfo)
			if len(infos) != 1 || infos[0].attrs["keys"] != "0" {
				t.Errorf("info = %+v, want one line with keys 0", infos)
			}
		})
	}
}

func TestWatchTakesOverAChangedFile(t *testing.T) {
	k1, l1 := newKey(t)
	k2, l2 := newKey(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	writeFile(t, path, l1+"\n")
	log, c := newLog()
	l, err := Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	changes := countChanges(l)
	watch(t, l)
	if _, ok := l.Lookup(k2); ok {
		t.Fatal("k2 found before it is in the file")
	}
	writeFile(t, path, l1+"\nread-only "+l2+" two\nrestrict "+l2+"\n")
	waitFor(t, "k2", func() bool { _, ok := l.Lookup(k2); return ok })
	waitReads(t, l)
	if n := changes.Load(); n != 1 {
		t.Errorf("OnChange called %d times, want 1", n)
	}
	if g, _ := l.Lookup(k2); !g.ReadOnly || g.Name != "two" {
		t.Errorf("k2 = %+v", g)
	}
	if _, ok := l.Lookup(k1); !ok {
		t.Error("k1 lost")
	}
	warns := c.at(slog.LevelWarn)
	if len(warns) != 1 || warns[0].attrs["line"] != "3" {
		t.Errorf("warnings = %+v, want line 3", warns)
	}
	infos := c.at(slog.LevelInfo)
	if len(infos) != 2 || infos[1].attrs["keys"] != "2" || infos[1].attrs["rejected"] != "1" {
		t.Errorf("info = %+v, want a second line with keys 2, rejected 1", infos)
	}
}

func TestWatchIgnoresAnUnchangedFile(t *testing.T) {
	key, line := newKey(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	content := line + " me\n"
	writeFile(t, path, content)
	log, _ := newLog()
	l, err := Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	changes := countChanges(l)
	watch(t, l)
	waitReads(t, l)
	// A new file with the same bytes: another inode and another time.
	writeFile(t, path, content)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	waitReads(t, l)
	if n := changes.Load(); n != 0 {
		t.Errorf("OnChange called %d times, want 0", n)
	}
	if _, ok := l.Lookup(key); !ok {
		t.Error("the key is lost")
	}
}

func TestWatchUnreadableFileIsEmpty(t *testing.T) {
	key, line := newKey(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	content := line + " me\n"
	writeFile(t, path, content)
	log, c := newLog()
	l, err := Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	changes := countChanges(l)
	watch(t, l)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the key to go", func() bool { _, ok := l.Lookup(key); return !ok })
	waitReads(t, l)
	if n := changes.Load(); n != 1 {
		t.Errorf("OnChange called %d times, want 1", n)
	}
	if errs := c.at(slog.LevelError); len(errs) != 1 {
		t.Errorf("errors logged = %+v, want one", errs)
	}
	writeFile(t, path, content)
	waitFor(t, "the key to come back", func() bool { _, ok := l.Lookup(key); return ok })
	waitReads(t, l)
	if n := changes.Load(); n != 2 {
		t.Errorf("OnChange called %d times, want 2", n)
	}
}

func TestWatchFollowsASwappedSymlink(t *testing.T) {
	k1, l1 := newKey(t)
	k2, l2 := newKey(t)
	dir := t.TempDir()
	for name, line := range map[string]string{"v1": l1, "v2": l2} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, name, "authorized_keys"), line+"\n")
	}
	if err := os.Symlink("v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	log, _ := newLog()
	l, err := Open(filepath.Join(dir, "..data", "authorized_keys"), log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Lookup(k1); !ok {
		t.Fatal("k1 not found")
	}
	watch(t, l)
	if err := os.Symlink("v2", filepath.Join(dir, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "k2", func() bool { _, ok := l.Lookup(k2); return ok })
	if _, ok := l.Lookup(k1); ok {
		t.Error("k1 still found")
	}
}

func TestWatchEndsWithItsContext(t *testing.T) {
	t.Run("a list that Open made", func(t *testing.T) {
		_, line := newKey(t)
		path := filepath.Join(t.TempDir(), "authorized_keys")
		writeFile(t, path, line+"\n")
		log, _ := newLog()
		l, err := Open(path, log)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			l.Watch(ctx, 5*time.Millisecond)
		}()
		waitReads(t, l)
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Watch did not return")
		}
	})
	t.Run("a list that NewList made", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			NewList(nil).Watch(context.Background(), 5*time.Millisecond)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Watch did not return")
		}
	})
}

func TestWatchWithoutAnIntervalReturns(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			_, line := newKey(t)
			path := filepath.Join(t.TempDir(), "authorized_keys")
			writeFile(t, path, line+"\n")
			log, _ := newLog()
			l, err := Open(path, log)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan any)
			go func() {
				defer func() { done <- recover() }()
				l.Watch(context.Background(), interval)
			}()
			select {
			case p := <-done:
				if p != nil {
					t.Fatalf("Watch panicked: %v", p)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Watch did not return")
			}
			if n := l.reads.Load(); n != 0 {
				t.Errorf("Watch read the file %d times", n)
			}
		})
	}
}
