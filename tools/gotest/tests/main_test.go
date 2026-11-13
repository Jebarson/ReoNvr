package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverlayMapsColocatedTestFolders(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{"internal/example/tests/example_test.go", "internal/example/example.go", "runtime/tests/ignored_test.go", "web/node_modules/example/tests/ignored_test.go"} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package example\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	overlay, err := testOverlay(root)
	if err != nil {
		t.Fatal(err)
	}
	virtual := filepath.Join(root, "internal/example/example_test.go")
	if len(overlay) != 1 || overlay[virtual] != filepath.Join(root, "internal/example/tests/example_test.go") {
		t.Fatalf("unexpected overlay: %v", overlay)
	}
	if err := os.WriteFile(virtual, []byte("package example\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := testOverlay(root); err == nil {
		t.Fatal("an existing package test was silently replaced")
	}
}
