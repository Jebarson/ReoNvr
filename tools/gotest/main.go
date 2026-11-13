package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func testOverlay(root string) (map[string]string, error) {
	replacements := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "runtime":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) != "tests" {
			return fmt.Errorf("Go tests must live in an adjacent tests/ directory: %s", path)
		}
		virtual := filepath.Join(filepath.Dir(filepath.Dir(path)), entry.Name())
		if _, err := os.Stat(virtual); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("test overlay destination must not exist: %s", virtual)
		}
		replacements[virtual] = path
		return nil
	})
	return replacements, err
}

func run(arguments []string) error {
	module, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return err
	}
	root := filepath.Dir(strings.TrimSpace(string(module)))
	replacements, err := testOverlay(root)
	if err != nil {
		return err
	}
	if len(replacements) == 0 {
		return errors.New("no Go tests found in package tests/ directories")
	}
	temporary, err := os.MkdirTemp("", "reonvr-go-tests-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	payload, err := json.Marshal(struct {
		Replace map[string]string
	}{Replace: replacements})
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(temporary, "overlay.json")
	if err := os.WriteFile(overlayPath, payload, 0o600); err != nil {
		return err
	}
	operation := "test"
	if len(arguments) > 0 && (arguments[0] == "test" || arguments[0] == "vet") {
		operation, arguments = arguments[0], arguments[1:]
	}
	if len(arguments) == 0 {
		arguments = []string{"./..."}
	}
	command := exec.Command("go", append([]string{operation, "-overlay", overlayPath}, arguments...)...)
	command.Dir = root
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}
