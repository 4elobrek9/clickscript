package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileKeepsRecentBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.txt")
	writer, err := Open(path, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, line := range []string{"1111\n", "2222\n", "3333\n", "4444\n"} {
		if _, err := writer.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{
		"log.txt":   "4444\n",
		"log.txt.1": "3333\n",
		"log.txt.2": "2222\n",
	} {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q; want %q", name, got, want)
		}
	}
}
