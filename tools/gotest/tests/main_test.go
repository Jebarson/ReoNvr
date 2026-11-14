package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestRuntimeEventsRequireEveryNamedTestAndPackagePass(t *testing.T) {
	first := `{"Action":"pass","Test":"First"}` + "\n"
	second := `{"Action":"pass","Test":"Second"}` + "\n"
	packagePass := `{"Action":"pass"}` + "\n"
	for _, testCase := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{"complete", first + second + packagePass, true},
		{"no tests", packagePass, false},
		{"missing test", first + packagePass, false},
		{"skipped test", first + `{"Action":"skip","Test":"Second"}` + "\n" + packagePass, false},
		{"failed test", first + `{"Action":"fail","Test":"Second"}` + "\n", false},
		{"failed package", first + second + `{"Action":"fail"}` + "\n", false},
		{"incomplete output", first + second, false},
		{"malformed", "not JSON", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := verifyTestEvents(strings.NewReader(testCase.payload), []string{"First", "Second"})
			if (err == nil) != testCase.valid {
				t.Fatalf("unexpected event validation result: %v", err)
			}
		})
	}
	if err := verifyTestEvents(strings.NewReader(packagePass), nil); err == nil {
		t.Fatal("empty required-test list accepted")
	}
}
