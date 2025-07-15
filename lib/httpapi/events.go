package httpapi

import (
	"fmt"
	"strings"
	"sync"
	"time"

	mf "github.com/coder/agentapi/lib/msgfmt"
	st "github.com/coder/agentapi/lib/screentracker"
	"github.com/coder/agentapi/lib/util"
	"github.com/danielgtaylor/huma/v2"
)

// EventType is the type of an event.
type EventType string

// Event types.
const (
	EventTypeMessageUpdate EventType = "message_update"
	EventTypeStatusChange  EventType = "status_change"
	EventTypeScreenUpdate  EventType = "screen_update"
)

// AgentStatus is the status of the agent.
type AgentStatus string

// Agent statuses.
const (
	AgentStatusRunning AgentStatus = "running"
	AgentStatusStable  AgentStatus = "stable"
)

// AgentStatusValues are the possible values for AgentStatus.
var AgentStatusValues = []AgentStatus{
	AgentStatusStable,
	AgentStatusRunning,
}

// Schema returns the OpenAPI schema for AgentStatus.
func (a AgentStatus) Schema(r huma.Registry) *huma.Schema {
	return util.OpenAPISchema(r, "AgentStatus", AgentStatusValues)
}

// MessageUpdateBody is the payload for a message update event.
type MessageUpdateBody struct {
	ID      int                 `json:"id" doc:"Unique identifier for the message. This identifier also represents the order of the message in the conversation history."`
	Role    st.ConversationRole `json:"role" doc:"Role of the message author"`
	Message string              `json:"message" doc:"Message content. The message is formatted as it appears in the agent's terminal session, meaning that, by default, it consists of lines of text with 80 characters per line."`
	Time    time.Time           `json:"time" doc:"Timestamp of the message"`
}

// StatusChangeBody is the payload for a status change event.
type StatusChangeBody struct {
	Status AgentStatus `json:"status" doc:"Agent status"`
}

// ScreenUpdateBody is the payload for a screen update event.
type ScreenUpdateBody struct {
	Screen string `json:"screen"`
}

// Event is an event that can be sent to a client.
type Event struct {
	Type    EventType
	Payload any
}

// EventEmitter is an event emitter that sends events to subscribers.
type EventEmitter struct {
	mu                  sync.Mutex
	messages            []st.ConversationMessage
	status              AgentStatus
	chans               map[int]chan Event
	chanIdx             int
	subscriptionBufSize int
	screen              string
}

func convertStatus(status st.ConversationStatus) AgentStatus {
	switch status {
	case st.ConversationStatusInitializing:
		return AgentStatusRunning
	case st.ConversationStatusStable:
		return AgentStatusStable
	case st.ConversationStatusChanging:
		return AgentStatusRunning
	default:
		panic(fmt.Sprintf("unknown conversation status: %s", status))
	}
}

// NewEventEmitter creates a new EventEmitter.
// subscriptionBufSize is the size of the buffer for each subscription.
// Once the buffer is full, the channel will be closed.
// Listeners must actively drain the channel, so it's important to
// set this to a value that is large enough to handle the expected
// number of events.
func NewEventEmitter(subscriptionBufSize int) *EventEmitter {
	return &EventEmitter{
		mu:                  sync.Mutex{},
		messages:            make([]st.ConversationMessage, 0),
		status:              AgentStatusRunning,
		chans:               make(map[int]chan Event),
		chanIdx:             0,
		subscriptionBufSize: subscriptionBufSize,
	}
}

// Assumes the caller holds the lock.
func (e *EventEmitter) notifyChannels(eventType EventType, payload any) {
	chanIDs := make([]int, 0, len(e.chans))
	for chanID := range e.chans {
		chanIDs = append(chanIDs, chanID)
	}
	for _, chanID := range chanIDs {
		ch := e.chans[chanID]
		event := Event{
			Type:    eventType,
			Payload: payload,
		}

		select {
		case ch <- event:
		default:
			// If the channel is full, close it.
			// Listeners must actively drain the channel.
			e.unsubscribeInner(chanID)
		}
	}
}

// UpdateMessagesAndEmitChanges updates the messages and emits changes to subscribers.
// Assumes that only the last message can change or new messages can be added.
// If a new message is injected between existing messages (identified by Id), the behavior is undefined.
func (e *EventEmitter) UpdateMessagesAndEmitChanges(newMessages []st.ConversationMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()

	maxLength := max(len(e.messages), len(newMessages))
	for i := range maxLength {
		var oldMsg st.ConversationMessage
		var newMsg st.ConversationMessage
		if i < len(e.messages) {
			oldMsg = e.messages[i]
		}
		if i < len(newMessages) {
			newMsg = newMessages[i]
		}
		if oldMsg != newMsg {
			e.notifyChannels(EventTypeMessageUpdate, MessageUpdateBody{
				ID:      newMessages[i].ID,
				Role:    newMessages[i].Role,
				Message: newMessages[i].Message,
				Time:    newMessages[i].Time,
			})
		}
	}

	e.messages = newMessages
}

// UpdateStatusAndEmitChanges updates the status and emits changes to subscribers.
func (e *EventEmitter) UpdateStatusAndEmitChanges(newStatus st.ConversationStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()

	newAgentStatus := convertStatus(newStatus)
	if e.status == newAgentStatus {
		return
	}

	e.notifyChannels(EventTypeStatusChange, StatusChangeBody{Status: newAgentStatus})
	e.status = newAgentStatus
}

// UpdateScreenAndEmitChanges updates the screen and emits changes to subscribers.
func (e *EventEmitter) UpdateScreenAndEmitChanges(newScreen string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.screen == newScreen {
		return
	}

	e.notifyChannels(EventTypeScreenUpdate, ScreenUpdateBody{Screen: strings.TrimRight(newScreen, mf.WhiteSpaceChars)})
	e.screen = newScreen
}

// Assumes the caller holds the lock.
func (e *EventEmitter) currentStateAsEvents() []Event {
	events := make([]Event, 0, len(e.messages)+2)
	for _, msg := range e.messages {
		events = append(events, Event{
			Type:    EventTypeMessageUpdate,
			Payload: MessageUpdateBody{ID: msg.ID, Role: msg.Role, Message: msg.Message, Time: msg.Time},
		})
	}
	events = append(events, Event{
		Type:    EventTypeStatusChange,
		Payload: StatusChangeBody{Status: e.status},
	})
	events = append(events, Event{
		Type:    EventTypeScreenUpdate,
		Payload: ScreenUpdateBody{Screen: strings.TrimRight(e.screen, mf.WhiteSpaceChars)},
	})
	return events
}

// Subscribe returns:
// - a subscription ID that can be used to unsubscribe.
// - a channel for receiving events.
// - a list of events that allow to recreate the state of the conversation right before the subscription was created.
func (e *EventEmitter) Subscribe() (int, <-chan Event, []Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stateEvents := e.currentStateAsEvents()

	// Once a channel becomes full, it will be closed.
	ch := make(chan Event, e.subscriptionBufSize)
	e.chans[e.chanIdx] = ch
	e.chanIdx++
	return e.chanIdx - 1, ch, stateEvents
}

// Assumes the caller holds the lock.
func (e *EventEmitter) unsubscribeInner(chanID int) {
	close(e.chans[chanID])
	delete(e.chans, chanID)
}

// Unsubscribe unsubscribes a client from receiving events.
func (e *EventEmitter) Unsubscribe(chanID int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unsubscribeInner(chanID)
}
