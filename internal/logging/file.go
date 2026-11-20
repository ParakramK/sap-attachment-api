package logging

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dailyRotator appends to logs/<app>-YYYY-MM-DD.log and rolls to a fresh
// file when the local date changes. No dependency, no pruning: old files
// stay until removed externally.
type dailyRotator struct {
	mu   sync.Mutex
	dir  string
	day  string
	file *os.File
	now  func() time.Time
}

func newDailyRotator(dir string) (*dailyRotator, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	r := &dailyRotator{dir: dir, now: time.Now}
	if err := r.rotate(r.now()); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *dailyRotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.rotate(r.now()); err != nil {
		return 0, err
	}
	return r.file.Write(p)
}

func (r *dailyRotator) rotate(now time.Time) error {
	day := now.Format("2006-01-02")
	if r.file != nil && day == r.day {
		return nil
	}
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	f, err := os.OpenFile( //nolint:gosec // G304: path is internally derived from the executable dir and date
		filepath.Join(r.dir, "sap-attachment-api-"+day+".log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600,
	)
	if err != nil {
		return err
	}
	r.file, r.day = f, day
	return nil
}

func (r *dailyRotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// executableLogsDir returns <exe dir>/logs, following symlinks so a launcher
// symlink still writes beside the real binary.
func executableLogsDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "logs"), nil
}
