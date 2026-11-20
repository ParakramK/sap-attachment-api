package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyRotatorCreatesDatedFile(t *testing.T) {
	dir := t.TempDir()
	r, err := newDailyRotator(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("first line\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dir, "sap-attachment-api-"+time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(want) //nolint:gosec // G304: test-owned temp path
	if err != nil || string(data) != "first line\n" {
		t.Fatalf("file %q = %q, err %v", want, data, err)
	}
}

func TestDailyRotatorRollsOnDateChange(t *testing.T) {
	dir := t.TempDir()
	r, err := newDailyRotator(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("today\n")); err != nil {
		t.Fatal(err)
	}

	// Move the clock to tomorrow; the next write must open a new dated file.
	tomorrow := time.Now().AddDate(0, 0, 1)
	r.mu.Lock()
	r.now = func() time.Time { return tomorrow }
	r.mu.Unlock()
	if _, err := r.Write([]byte("rolled\n")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "sap-attachment-api-*.log"))
	if err != nil || len(files) != 2 {
		t.Fatalf("files = %v, err %v", files, err)
	}
	var found bool
	for _, f := range files {
		data, _ := os.ReadFile(f) //nolint:gosec // G304: test-owned temp path
		if strings.Contains(string(data), "rolled") {
			found = true
		}
	}
	if !found {
		t.Fatal("rolled log line not written to a new file")
	}
}
