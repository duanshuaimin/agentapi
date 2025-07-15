package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/coder/agentapi/cmd/attach"
	"github.com/coder/agentapi/cmd/server"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "agentapi",
	Short:   "AgentAPI CLI",
	Long:    `AgentAPI - HTTP API for Claude Code, Goose, Aider, and Codex`,
	Version: "0.2.3",
}

// Execute executes the root command.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(ensureConfigFile)
	rootCmd.AddCommand(server.ServerCmd)
	rootCmd.AddCommand(attach.AttachCmd)
}

// ensureConfigFile checks if .claude.json exists in the user's home directory.
// If it doesn't exist, it copies it from the current working directory.
func ensureConfigFile() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting user home directory: %v\n", err)
		return
	}

	configFilePath := filepath.Join(homeDir, ".claude.json")

	// Check if the file already exists
	if _, err := os.Stat(configFilePath); err == nil {
		// File exists, do nothing.
		return
	} else if !os.IsNotExist(err) {
		// Another error occurred (e.g., permission denied)
		fmt.Fprintf(os.Stderr, "Error checking config file %s: %v\n", configFilePath, err)
		return
	}

	// File does not exist, so copy it.
	sourcePath := ".claude.json"

	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		// The source file does not exist in the current directory
		// This is not a fatal error, as the user may provide a config path via a flag.
		// Or the program might not need the config file at all.
		return
	}

	fmt.Printf("Config file not found at %s. Copying from current directory...\n", configFilePath)

	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening source config file %s: %v\n", sourcePath, err)
		return
	}
	defer sourceFile.Close()

	destinationFile, err := os.Create(configFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating destination config file %s: %v\n", configFilePath, err)
		return
	}
	defer destinationFile.Close()

	_, err = io.Copy(destinationFile, sourceFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error copying config file: %v\n", err)
		return
	}

	fmt.Printf("Successfully copied .claude.json to %s\n", configFilePath)
}
