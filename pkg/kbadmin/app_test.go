package kbadmin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallToFile_CreatesNewTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")

	if err := InstallToFile(target); err != nil {
		t.Fatalf("InstallToFile: %v", err)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if strings.TrimSpace(string(content)) != "@kb.md" {
		t.Errorf("target content = %q, want just '@kb.md'", string(content))
	}

	kbMd, err := os.ReadFile(filepath.Join(dir, "kb.md"))
	if err != nil {
		t.Fatalf("read kb.md: %v", err)
	}
	if string(kbMd) != installSnippet {
		t.Errorf("kb.md content = %q, want snippet", string(kbMd))
	}
}

func TestInstallToFile_AppendsToExistingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	existing := "# Existing content\n\nSome rules.\n"
	if err := os.WriteFile(target, []byte(existing), 0644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	if err := InstallToFile(target); err != nil {
		t.Fatalf("InstallToFile: %v", err)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.HasPrefix(string(content), existing) {
		t.Errorf("existing content not preserved: %q", string(content))
	}
	if !strings.Contains(string(content), "@kb.md") {
		t.Errorf("@kb.md not appended: %q", string(content))
	}
}

func TestInstallToFile_Idempotent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")

	if err := InstallToFile(target); err != nil {
		t.Fatalf("first InstallToFile: %v", err)
	}
	first, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}

	if err := InstallToFile(target); err != nil {
		t.Fatalf("second InstallToFile: %v", err)
	}
	second, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("content changed on re-run:\nfirst:  %q\nsecond: %q", string(first), string(second))
	}
	if n := strings.Count(string(second), "@kb.md"); n != 1 {
		t.Errorf("@kb.md appears %d times, want 1", n)
	}
}

func TestInstallToFile_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.md")
	if err := os.WriteFile(real, []byte("real\n"), 0644); err != nil {
		t.Fatalf("seed real: %v", err)
	}
	link := filepath.Join(dir, "AGENTS.md")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := InstallToFile(link)
	if err == nil {
		t.Fatal("InstallToFile succeeded on symlink, want error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %v, want mention of symlink", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "kb.md")); err == nil {
		t.Error("kb.md was written despite symlink rejection")
	}
}

func TestInstallToFile_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := InstallToFile(target); err == nil {
		t.Fatal("InstallToFile succeeded on directory, want error")
	}
}

func TestInstallToFile_OverwritesExistingKbMd(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(filepath.Join(dir, "kb.md"), []byte("stale\n"), 0644); err != nil {
		t.Fatalf("seed kb.md: %v", err)
	}

	if err := InstallToFile(target); err != nil {
		t.Fatalf("InstallToFile: %v", err)
	}

	kbMd, err := os.ReadFile(filepath.Join(dir, "kb.md"))
	if err != nil {
		t.Fatalf("read kb.md: %v", err)
	}
	if string(kbMd) != installSnippet {
		t.Errorf("kb.md not overwritten: %q", string(kbMd))
	}
}

func TestInstallToFile_RecognizesExistingIncludeLine(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	existing := "# Rules\n\n@kb.md\n\nMore stuff.\n"
	if err := os.WriteFile(target, []byte(existing), 0644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	if err := InstallToFile(target); err != nil {
		t.Fatalf("InstallToFile: %v", err)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(content) != existing {
		t.Errorf("target modified despite existing @kb.md: %q", string(content))
	}
	if n := strings.Count(string(content), "@kb.md"); n != 1 {
		t.Errorf("@kb.md appears %d times, want 1", n)
	}
}
