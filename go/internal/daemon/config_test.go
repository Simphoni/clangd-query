package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Writes the given content as the project configuration file inside dir and
// returns the full path of the written file.
func writeConfigFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	return path
}

// Covers the loading behavior for the common configurations: missing file,
// empty object, relative and absolute compile database paths, and the extra
// clangd arguments.
func TestLoadProjectConfig(t *testing.T) {
	t.Run("missing file yields nil config", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := LoadProjectConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Fatalf("expected nil config for missing file, got %+v", cfg)
		}
	})

	t.Run("empty object yields zero-value config", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigFile(t, dir, `{}`)
		cfg, err := LoadProjectConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config for existing file")
		}
		if cfg.CompileCommands != "" || cfg.Generate != "" || len(cfg.ClangdArgs) != 0 {
			t.Fatalf("expected zero-value fields, got %+v", cfg)
		}
	})

	t.Run("relative compile commands path is resolved against project root", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigFile(t, dir, `{
			"compileCommands": "build/compile_commands.json",
			"generate": "cmake --preset local",
			"clangdArgs": ["--query-driver=/usr/bin/arm-none-eabi-*"]
		}`)
		cfg, err := LoadProjectConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := filepath.Join(dir, "build", "compile_commands.json")
		if cfg.CompileCommands != expected {
			t.Fatalf("expected CompileCommands %q, got %q", expected, cfg.CompileCommands)
		}
		if cfg.Generate != "cmake --preset local" {
			t.Fatalf("unexpected Generate value: %q", cfg.Generate)
		}
		if len(cfg.ClangdArgs) != 1 || cfg.ClangdArgs[0] != "--query-driver=/usr/bin/arm-none-eabi-*" {
			t.Fatalf("unexpected ClangdArgs: %v", cfg.ClangdArgs)
		}
	})

	t.Run("absolute compile commands path is preserved", func(t *testing.T) {
		dir := t.TempDir()
		abs := filepath.Join(string(filepath.Separator), "elsewhere", "compile_commands.json")
		writeConfigFile(t, dir, `{"compileCommands": "`+abs+`"}`)
		cfg, err := LoadProjectConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.CompileCommands != abs {
			t.Fatalf("expected CompileCommands %q, got %q", abs, cfg.CompileCommands)
		}
	})
}

// Covers the failure modes: malformed JSON and a generate command without a
// declared output location. Both must surface as hard errors rather than
// silently falling back to defaults.
func TestLoadProjectConfigErrors(t *testing.T) {
	t.Run("malformed JSON is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigFile(t, dir, `{not json`)
		if _, err := LoadProjectConfig(dir); err == nil {
			t.Fatal("expected error for malformed JSON")
		}
	})

	t.Run("generate without compileCommands is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigFile(t, dir, `{"generate": "cmake --preset local"}`)
		_, err := LoadProjectConfig(dir)
		if err == nil {
			t.Fatal("expected error when generate is set without compileCommands")
		}
		if !strings.Contains(err.Error(), "compileCommands") {
			t.Fatalf("error should mention missing compileCommands, got: %v", err)
		}
	})
}

// Verifies that the mtime helper reports 0 for absent files and a non-zero
// value matching the file's actual modification time when it exists, since the
// staleness check compares this value against what the daemon recorded in its
// lock file.
func TestConfigMTime(t *testing.T) {
	dir := t.TempDir()

	if mtime := ConfigMTime(dir); mtime != 0 {
		t.Fatalf("expected 0 for missing config file, got %d", mtime)
	}

	path := writeConfigFile(t, dir, `{}`)
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat config file: %v", err)
	}
	if mtime := ConfigMTime(dir); mtime != stat.ModTime().Unix() {
		t.Fatalf("expected mtime %d, got %d", stat.ModTime().Unix(), mtime)
	}
}
