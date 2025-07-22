package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"golang.org/x/xerrors"

	"github.com/coder/agentapi/lib/httpapi"
	"github.com/coder/agentapi/lib/logctx"
	"github.com/coder/agentapi/lib/msgfmt"
	"github.com/coder/agentapi/lib/termexec"
)

var (
	agentTypeVar string
	port         int
	printOpenAPI bool
	chatBasePath string
	termWidth    uint16
	termHeight   uint16
	logLevel     string
)

// AgentType is the type of agent to run.
type AgentType = msgfmt.AgentType

// Agent types.
const (
	AgentTypeClaude AgentType = msgfmt.AgentTypeClaude
	AgentTypeGoose  AgentType = msgfmt.AgentTypeGoose
	AgentTypeAider  AgentType = msgfmt.AgentTypeAider
	AgentTypeCodex  AgentType = msgfmt.AgentTypeCodex
	AgentTypeCustom AgentType = msgfmt.AgentTypeCustom
)

func parseLogLevel(levelStr string) slog.Level {
	switch strings.ToUpper(levelStr) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo // 默认为 INFO 级别
	}
}

func parseAgentType(firstArg string, agentTypeVar string) (AgentType, error) {
	var agentType AgentType
	switch agentTypeVar {
	case string(AgentTypeClaude):
		agentType = AgentTypeClaude
	case string(AgentTypeGoose):
		agentType = AgentTypeGoose
	case string(AgentTypeAider):
		agentType = AgentTypeAider
	case string(AgentTypeCustom):
		agentType = AgentTypeCustom
	case string(AgentTypeCodex):
		agentType = AgentTypeCodex
	case "":
		// do nothing
	default:
		return "", fmt.Errorf("invalid agent type: %s", agentTypeVar)
	}
	if agentType != "" {
		return agentType, nil
	}

	switch firstArg {
	case string(AgentTypeClaude):
		agentType = AgentTypeClaude
	case string(AgentTypeGoose):
		agentType = AgentTypeGoose
	case string(AgentTypeAider):
		agentType = AgentTypeAider
	case string(AgentTypeCodex):
		agentType = AgentTypeCodex
	default:
		agentType = AgentTypeCustom
	}
	return agentType, nil
}

var errAgentFound = errors.New("agent found")

func findAgentPath(agent string) (string, error) {
	// If the path is absolute, just check if it exists.
	if filepath.IsAbs(agent) {
		if _, err := os.Stat(agent); err == nil {
			return agent, nil
		}
		return "", fmt.Errorf("agent not found at absolute path: %s", agent)
	}

	// Check if the agent is in the system's PATH.
	if path, err := exec.LookPath(agent); err == nil {
		return path, nil
	}

	// Search in user's home directory recursively.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", xerrors.Errorf("could not get user home directory: %w", err)
	}

	var foundPath string
	walkErr := filepath.WalkDir(homeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Don't walk into directories we can't read
			if os.IsPermission(err) {
				return filepath.SkipDir
			}
			return err
		}
		// Skip directories that are common for dependencies or caches to speed up search.
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", ".cache", "vendor", "Library", "AppData", "Caches", "logs", "Logs":
				return filepath.SkipDir
			}
		}

		if !d.IsDir() && d.Name() == agent {
			if info, err := d.Info(); err == nil {
				if info.Mode()&0111 != 0 {
					foundPath = path
					return errAgentFound // Stop searching
				}
			}
		}
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, errAgentFound) {
		return "", xerrors.Errorf("error while searching for agent in home directory: %w", walkErr)
	}

	if foundPath != "" {
		return foundPath, nil
	}

	return "", fmt.Errorf("agent executable '%s' not found in PATH or user's home directory", agent)
}

func ensureClaudeOnboarding(logger *slog.Logger) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return xerrors.Errorf("could not get user home directory: %w", err)
	}
	configPath := filepath.Join(homeDir, ".claude.json")

	var config map[string]interface{}

	fileData, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist, create a new one
			config = make(map[string]interface{})
		} else {
			return xerrors.Errorf("failed to read claude config file: %w", err)
		}
	} else {
		// File exists, parse it
		if err := json.Unmarshal(fileData, &config); err != nil {
			return xerrors.Errorf("failed to parse claude config file: %w", err)
		}
	}

	// Check if onboarding is complete
	if val, ok := config["hasCompletedOnboarding"].(bool); ok && val {
		logger.Info("Claude onboarding already completed.")
		return nil
	}

	// Update the config and write it back
	logger.Info("Setting hasCompletedOnboarding to true in claude config.")
	config["hasCompletedOnboarding"] = true

	updatedData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return xerrors.Errorf("failed to marshal updated claude config: %w", err)
	}

	if err := os.WriteFile(configPath, updatedData, 0644); err != nil {
		return xerrors.Errorf("failed to write updated claude config: %w", err)
	}

	return nil
}

func runServer(ctx context.Context, logger *slog.Logger, argsToPass []string) error {
	agentName := argsToPass[0]
	logger.Info("Starting agent", "agent", agentName)

	agentPath, err := findAgentPath(agentName)
	if err != nil {
		return xerrors.Errorf("failed to find agent executable: %w", err)
	}
	logger.Info("Found agent executable", "path", agentPath)

	agentType, err := parseAgentType(agentName, agentTypeVar)
	if err != nil {
		return xerrors.Errorf("failed to parse agent type: %w", err)
	}

	if agentType == AgentTypeClaude {
		if err := ensureClaudeOnboarding(logger); err != nil {
			return xerrors.Errorf("failed to ensure claude onboarding: %w", err)
		}
		err := godotenv.Load()
		if err != nil {
			return xerrors.Errorf("failed to load .env file: %w", err)
		}
	}

	if termWidth < 10 {
		return xerrors.Errorf("term width must be at least 10")
	}
	if termHeight < 10 {
		return xerrors.Errorf("term height must be at least 10")
	}

	var process *termexec.Process
	if printOpenAPI {
		process = nil
	} else {
		process, err = httpapi.SetupProcess(ctx, httpapi.SetupProcessConfig{
			Program:        agentPath,
			ProgramArgs:    argsToPass[1:],
			TerminalWidth:  termWidth,
			TerminalHeight: termHeight,
		})
		if err != nil {
			return xerrors.Errorf("failed to setup process: %w", err)
		}
	}
	srv := httpapi.NewServer(ctx, agentType, process, port, chatBasePath)
	if printOpenAPI {
		fmt.Println(srv.GetOpenAPI())
		return nil
	}
	srv.StartSnapshotLoop(ctx)
	logger.Info("Starting server on port", "port", port)
	processExitCh := make(chan error, 1)
	go func() {
		defer close(processExitCh)
		if err := process.Wait(); err != nil {
			if errors.Is(err, termexec.ErrNonZeroExitCode) {
				processExitCh <- xerrors.Errorf("========\n%s\n========\n: %w", strings.TrimSpace(process.ReadScreen()), err)
			} else {
				processExitCh <- xerrors.Errorf("failed to wait for process: %w", err)
			}
		}
		if err := srv.Stop(ctx); err != nil {
			logger.Error("Failed to stop server", "error", err)
		}
	}()
	if err := srv.Start(); err != nil && err != context.Canceled && err != http.ErrServerClosed {
		return xerrors.Errorf("failed to start server: %w", err)
	}
	select {
	case err := <-processExitCh:
		return xerrors.Errorf("agent exited with error: %w", err)
	default:
	}
	return nil
}

// ServerCmd is the command to run the server.
var ServerCmd = &cobra.Command{
	Use:   "server [agent]",
	Short: "Run the server",
	Long:  `Run the server with the specified agent (claude, goose, aider, codex)`,
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		level := parseLogLevel(logLevel)
		logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		}))
		ctx := logctx.WithLogger(context.Background(), logger)
		if err := runServer(ctx, logger, cmd.Flags().Args()); err != nil {
			fmt.Fprintf(os.Stderr, "%+v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	ServerCmd.Flags().StringVarP(&agentTypeVar, "type", "t", "", "Override the agent type (one of: claude, goose, aider, custom)")
	ServerCmd.Flags().IntVarP(&port, "port", "p", 3284, "Port to run the server on")
	ServerCmd.Flags().BoolVarP(&printOpenAPI, "print-openapi", "P", false, "Print the OpenAPI schema to stdout and exit")
	ServerCmd.Flags().StringVarP(&chatBasePath, "chat-base-path", "c", "/chat", "Base path for assets and routes used in the static files of the chat interface")
	ServerCmd.Flags().Uint16VarP(&termWidth, "term-width", "W", 80, "Width of the emulated terminal")
	ServerCmd.Flags().Uint16VarP(&termHeight, "term-height", "H", 1000, "Height of the emulated terminal")
	ServerCmd.Flags().StringVarP(&logLevel, "log-level", "l", "info", "Log level (debug, info, warn, error)")
}
