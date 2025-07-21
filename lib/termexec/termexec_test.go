package termexec

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStartProcess_FindsExecutableInSubdirectory(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("Failed to get home directory: %v", err)
	}

	claudeDir := filepath.Join(homeDir, ".claude", "test-subdir")
	if err := os.MkdirAll(claudeDir, 0755);
	err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}
	defer os.RemoveAll(filepath.Join(homeDir, ".claude"))

	executablePath := filepath.Join(claudeDir, "claude")
	if _, err := os.Create(executablePath);
	err != nil {
		t.Fatalf("Failed to create test executable: %v", err)
	}
	if err := os.Chmod(executablePath, 0755);
	err != nil {
		t.Fatalf("Failed to make test executable executable: %v", err)
	}

	config := StartProcessConfig{
		Program:        "claude",
		Args:           []string{"--version"},
		TerminalWidth:  80,
		TerminalHeight: 24,
	}

	cmd := exec.Command(config.Program, config.Args...)
	cmd.Env = append(os.Environ(), "TERM=vt100")

	newPath := claudeDir + ":" + os.Getenv("PATH")
	cmd.Env = append(cmd.Env, "PATH="+newPath)

	originalPath := os.Getenv("PATH")
	os.Setenv("PATH", newPath)
	defer os.Setenv("PATH", originalPath)

	path, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("Failed to find executable in custom path: %v", err)
	}

	if path != executablePath {
		t.Errorf("Expected to find executable at %s, but found it at %s", executablePath, path)
	}
}
