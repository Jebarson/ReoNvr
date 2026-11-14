package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func verifyTestEvents(reader io.Reader, required []string) error {
	if len(required) == 0 {
		return errors.New("at least one required test name is needed")
	}
	passed := make(map[string]bool, len(required))
	for _, name := range required {
		passed[name] = false
	}
	packagePassed := false
	decoder := json.NewDecoder(reader)
	for {
		var event struct {
			Action string
			Test   string
		}
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode test events: %w", err)
		}
		if event.Action == "fail" {
			return errors.New("runtime media validation failed")
		}
		if _, required := passed[event.Test]; required {
			if event.Action == "skip" {
				return fmt.Errorf("required test was skipped: %s", event.Test)
			}
			if event.Action == "pass" {
				passed[event.Test] = true
			}
		} else if event.Test == "" && event.Action == "pass" {
			packagePassed = true
		}
	}
	if !packagePassed {
		return errors.New("runtime test package did not report a pass")
	}
	for name, complete := range passed {
		if !complete {
			return fmt.Errorf("required test did not report a pass: %s", name)
		}
	}
	return nil
}

func run(arguments []string) error {
	if len(arguments) > 0 && arguments[0] == "verify" {
		return verifyTestEvents(os.Stdin, arguments[1:])
	}
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
