package screentracker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coder/agentapi/lib/msgfmt"
	"github.com/coder/agentapi/lib/util"
	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/xerrors"
)

type screenSnapshot struct {
	timestamp time.Time
	screen    string
}

// AgentIO is an interface for interacting with an agent.
type AgentIO interface {
	Write(data []byte) (int, error)
	ReadScreen() string
}

// ConversationConfig is the configuration for a conversation.
type ConversationConfig struct {
	AgentIO AgentIO
	// GetTime returns the current time
	GetTime func() time.Time
	// How often to take a snapshot for the stability check
	SnapshotInterval time.Duration
	// How long the screen should not change to be considered stable
	ScreenStabilityLength time.Duration
	// Function to format the messages received from the agent
	// userInput is the last user message
	FormatMessage func(message string, userInput string) string
	// SkipWritingMessage skips the writing of a message to the agent.
	// This is used in tests
	SkipWritingMessage bool
	// SkipSendMessageStatusCheck skips the check for whether the message can be sent.
	// This is used in tests
	SkipSendMessageStatusCheck bool
}

// ConversationRole is the role of a message author.
type ConversationRole string

// Conversation roles.
const (
	ConversationRoleUser  ConversationRole = "user"
	ConversationRoleAgent ConversationRole = "agent"
)

// ConversationRoleValues are the possible values for ConversationRole.
var ConversationRoleValues = []ConversationRole{
	ConversationRoleUser,
	ConversationRoleAgent,
}

// Schema returns the OpenAPI schema for ConversationRole.
func (c ConversationRole) Schema(r huma.Registry) *huma.Schema {
	return util.OpenAPISchema(r, "ConversationRole", ConversationRoleValues)
}

// ConversationMessage is a message in a conversation.
type ConversationMessage struct {
	ID      int
	Message string
	Role    ConversationRole
	Time    time.Time
}

// Conversation tracks the conversation with an agent.
type Conversation struct {
	cfg ConversationConfig
	// How many stable snapshots are required to consider the screen stable
	stableSnapshotsThreshold    int
	snapshotBuffer              *RingBuffer[screenSnapshot]
	messages                    []ConversationMessage
	screenBeforeLastUserMessage string
	lock                        sync.Mutex
}

// ConversationStatus is the status of a conversation.
type ConversationStatus string

// Conversation statuses.
const (
	ConversationStatusChanging     ConversationStatus = "changing"
	ConversationStatusStable       ConversationStatus = "stable"
	ConversationStatusInitializing ConversationStatus = "initializing"
)

func getStableSnapshotsThreshold(cfg ConversationConfig) int {
	length := cfg.ScreenStabilityLength.Milliseconds()
	interval := cfg.SnapshotInterval.Milliseconds()
	threshold := int(length / interval)
	if length%interval != 0 {
		threshold++
	}
	return threshold + 1
}

// NewConversation creates a new conversation.
func NewConversation(ctx context.Context, cfg ConversationConfig) *Conversation {
	threshold := getStableSnapshotsThreshold(cfg)
	c := &Conversation{
		cfg:                      cfg,
		stableSnapshotsThreshold: threshold,
		snapshotBuffer:           NewRingBuffer[screenSnapshot](threshold),
		messages: []ConversationMessage{
			{
				Message: "",
				Role:    ConversationRoleAgent,
				Time:    cfg.GetTime(),
			},
		},
	}
	return c
}

// StartSnapshotLoop starts a loop that takes snapshots of the screen.
func (c *Conversation) StartSnapshotLoop(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.cfg.SnapshotInterval):
				// It's important that we hold the lock while reading the screen.
				// There's a race condition that occurs without it:
				// 1. The screen is read
				// 2. Independently, SendMessage is called and takes the lock.
				// 3. AddSnapshot is called and waits on the lock.
				// 4. SendMessage modifies the terminal state, releases the lock
				// 5. AddSnapshot adds a snapshot from a stale screen
				c.lock.Lock()
				screen := c.cfg.AgentIO.ReadScreen()
				c.addSnapshotInner(screen)
				c.lock.Unlock()
			}
		}
	}()
}

// FindNewMessage finds the new message in the new screen.
func FindNewMessage(oldScreen, newScreen string) string {
	oldLines := strings.Split(oldScreen, "\n")
	newLines := strings.Split(newScreen, "\n")
	oldLinesMap := make(map[string]bool)
	for _, line := range oldLines {
		oldLinesMap[line] = true
	}
	firstNonMatchingLine := len(newLines)
	for i, line := range newLines {
		if !oldLinesMap[line] {
			firstNonMatchingLine = i
			break
		}
	}
	newSectionLines := newLines[firstNonMatchingLine:]

	// remove leading and trailing lines which are empty or have only whitespace
	startLine := 0
	endLine := len(newSectionLines) - 1
	for i := 0; i < len(newSectionLines); i++ {
		if strings.TrimSpace(newSectionLines[i]) != "" {
			startLine = i
			break
		}
	}
	for i := len(newSectionLines) - 1; i >= 0; i-- {
		if strings.TrimSpace(newSectionLines[i]) != "" {
			endLine = i
			break
		}
	}
	return strings.Join(newSectionLines[startLine:endLine+1], "\n")
}

func (c *Conversation) lastMessage(role ConversationRole) ConversationMessage {
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role == role {
			return c.messages[i]
		}
	}
	return ConversationMessage{}
}

// This function assumes that the caller holds the lock
func (c *Conversation) updateLastAgentMessage(screen string, timestamp time.Time) {
	agentMessage := FindNewMessage(c.screenBeforeLastUserMessage, screen)
	lastUserMessage := c.lastMessage(ConversationRoleUser)
	if c.cfg.FormatMessage != nil {
		agentMessage = c.cfg.FormatMessage(agentMessage, lastUserMessage.Message)
	}
	shouldCreateNewMessage := len(c.messages) == 0 || c.messages[len(c.messages)-1].Role == ConversationRoleUser
	lastAgentMessage := c.lastMessage(ConversationRoleAgent)
	if lastAgentMessage.Message == agentMessage {
		return
	}
	conversationMessage := ConversationMessage{
		Message: agentMessage,
		Role:    ConversationRoleAgent,
		Time:    timestamp,
	}
	if shouldCreateNewMessage {
		c.messages = append(c.messages, conversationMessage)
	} else {
		c.messages[len(c.messages)-1] = conversationMessage
	}
	c.messages[len(c.messages)-1].ID = len(c.messages) - 1
}

// assumes the caller holds the lock
func (c *Conversation) addSnapshotInner(screen string) {
	snapshot := screenSnapshot{
		timestamp: c.cfg.GetTime(),
		screen:    screen,
	}
	c.snapshotBuffer.Add(snapshot)
	c.updateLastAgentMessage(screen, snapshot.timestamp)
}

// AddSnapshot adds a snapshot of the screen to the conversation.
func (c *Conversation) AddSnapshot(screen string) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.addSnapshotInner(screen)
}

// MessagePart is a part of a message.
type MessagePart interface {
	Do(writer AgentIO) error
	String() string
}

// MessagePartText is a text part of a message.
type MessagePartText struct {
	Content string
	Alias   string
	Hidden  bool
}

// Do writes the content of the message part to the agent.
func (p MessagePartText) Do(writer AgentIO) error {
	_, err := writer.Write([]byte(p.Content))
	return err
}

func (p MessagePartText) String() string {
	if p.Hidden {
		return ""
	}
	if p.Alias != "" {
		return p.Alias
	}
	return p.Content
}

// PartsToString converts a list of message parts to a string.
func PartsToString(parts ...MessagePart) string {
	var sb strings.Builder
	for _, part := range parts {
		sb.WriteString(part.String())
	}
	return sb.String()
}

// ExecuteParts executes a list of message parts.
func ExecuteParts(writer AgentIO, parts ...MessagePart) error {
	for _, part := range parts {
		if err := part.Do(writer); err != nil {
			return xerrors.Errorf("failed to write message part: %w", err)
		}
	}
	return nil
}


func (c *Conversation) writeMessageWithConfirmation(ctx context.Context, messageParts ...MessagePart) error {
	if c.cfg.SkipWritingMessage {
		return nil
	}
	screenBeforeMessage := c.cfg.AgentIO.ReadScreen()
	if err := ExecuteParts(c.cfg.AgentIO, messageParts...); err != nil {
		return xerrors.Errorf("failed to write message: %w", err)
	}
	// wait for the screen to stabilize after the message is written
	if err := util.WaitFor(ctx, util.WaitTimeout{
		Timeout:     15 * time.Second,
		MinInterval: 50 * time.Millisecond,
		InitialWait: true,
	}, func() (bool, error) {
		screen := c.cfg.AgentIO.ReadScreen()
		if screen != screenBeforeMessage {
			time.Sleep(1 * time.Second)
			newScreen := c.cfg.AgentIO.ReadScreen()
			return newScreen == screen, nil
		}
		return false, nil
	}); err != nil {
		return xerrors.Errorf("failed to wait for screen to stabilize: %w", err)
	}

	// wait for the screen to change after the carriage return is written
	screenBeforeCarriageReturn := c.cfg.AgentIO.ReadScreen()
	lastCarriageReturnTime := time.Time{}
	if err := util.WaitFor(ctx, util.WaitTimeout{
		Timeout:     15 * time.Second,
		MinInterval: 25 * time.Millisecond,
	}, func() (bool, error) {
		// we don't want to spam additional carriage returns because the agent may process them
		// (aider does this), but we do want to retry sending one if nothing's
		// happening for a while
		if time.Since(lastCarriageReturnTime) >= 3*time.Second {
			lastCarriageReturnTime = time.Now()
			if _, err := c.cfg.AgentIO.Write([]byte("\r")); err != nil {
				return false, xerrors.Errorf("failed to write carriage return: %w", err)
			}
		}
		time.Sleep(25 * time.Millisecond)
		screen := c.cfg.AgentIO.ReadScreen()

		return screen != screenBeforeCarriageReturn, nil
	}); err != nil {
		return xerrors.Errorf("failed to wait for processing to start: %w", err)
	}

	return nil
}

// MessageValidationErrorWhitespace is an error that occurs when a message is not trimmed of leading and trailing whitespace.
var MessageValidationErrorWhitespace = xerrors.New("message must be trimmed of leading and trailing whitespace")
// MessageValidationErrorEmpty is an error that occurs when a message is empty.
var MessageValidationErrorEmpty = xerrors.New("message must not be empty")
// MessageValidationErrorChanging is an error that occurs when a message is sent while the agent is not waiting for user input.
var MessageValidationErrorChanging = xerrors.New("message can only be sent when the agent is waiting for user input")

// SendMessage sends a message to the agent.
func (c *Conversation) SendMessage(messageParts ...MessagePart) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if !c.cfg.SkipSendMessageStatusCheck && c.statusInner() != ConversationStatusStable {
		return MessageValidationErrorChanging
	}

	message := PartsToString(messageParts...)
	if message != msgfmt.TrimWhitespace(message) {
		// msgfmt formatting functions assume this
		return MessageValidationErrorWhitespace
	}
	if message == "" {
		// writeMessageWithConfirmation requires a non-empty message
		return MessageValidationErrorEmpty
	}

	screenBeforeMessage := c.cfg.AgentIO.ReadScreen()
	now := c.cfg.GetTime()
	c.updateLastAgentMessage(screenBeforeMessage, now)

	if err := c.writeMessageWithConfirmation(context.Background(), messageParts...); err != nil {
		return xerrors.Errorf("failed to send message: %w", err)
	}

	c.screenBeforeLastUserMessage = screenBeforeMessage
	c.messages = append(c.messages, ConversationMessage{
		ID:      len(c.messages),
		Message: message,
		Role:    ConversationRoleUser,
		Time:    now,
	})
	return nil
}

// Assumes that the caller holds the lock
func (c *Conversation) statusInner() ConversationStatus {
	// sanity checks
	if c.snapshotBuffer.Capacity() != c.stableSnapshotsThreshold {
		panic(fmt.Sprintf("snapshot buffer capacity %d is not equal to snapshot threshold %d. can't check stability", c.snapshotBuffer.Capacity(), c.stableSnapshotsThreshold))
	}
	if c.stableSnapshotsThreshold == 0 {
		panic("stable snapshots threshold is 0. can't check stability")
	}

	snapshots := c.snapshotBuffer.GetAll()
	if len(c.messages) > 0 && c.messages[len(c.messages)-1].Role == ConversationRoleUser {
		// if the last message is a user message then the snapshot loop hasn't
		// been triggered since the last user message, and we should assume
		// the screen is changing
		return ConversationStatusChanging
	}

	if len(snapshots) != c.stableSnapshotsThreshold {
		return ConversationStatusInitializing
	}

	for i := 1; i < len(snapshots); i++ {
		if snapshots[0].screen != snapshots[i].screen {
			return ConversationStatusChanging
		}
	}
	return ConversationStatusStable
}

// Status returns the status of the conversation.
func (c *Conversation) Status() ConversationStatus {
	c.lock.Lock()
	defer c.lock.Unlock()

	return c.statusInner()
}

// Messages returns the messages in the conversation.
func (c *Conversation) Messages() []ConversationMessage {
	c.lock.Lock()
	defer c.lock.Unlock()

	result := make([]ConversationMessage, len(c.messages))
	copy(result, c.messages)
	return result
}

// Screen returns the current screen of the agent.
func (c *Conversation) Screen() string {
	c.lock.Lock()
	defer c.lock.Unlock()

	snapshots := c.snapshotBuffer.GetAll()
	if len(snapshots) == 0 {
		return ""
	}
	return snapshots[len(snapshots)-1].screen
}
