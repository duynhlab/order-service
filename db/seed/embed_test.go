package seed

import (
	"io/fs"
	"testing"
)

// TestFSEmbedsSeed verifies the demo seed SQL is embedded and applied by the
// `seed` subcommand (which reads the "sql" subtree).
func TestFSEmbedsSeed(t *testing.T) {
	entries, err := fs.ReadDir(FS, "sql")
	if err != nil {
		t.Fatalf("ReadDir(sql): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no seed files embedded under sql/")
	}
}
