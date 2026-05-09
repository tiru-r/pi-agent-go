package model

import "encoding/json"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

type ContentType string

const (
	ContentTypeText       ContentType = "text"
	ContentTypeImage      ContentType = "image"
	ContentTypeToolUse    ContentType = "tool_use"
	ContentTypeToolResult ContentType = "tool_result"
	ContentTypeThinking   ContentType = "thinking"
)

type ContentBlock struct {
	Type ContentType `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *ImageSource `json:"source,omitempty"`

	// tool_use (assistant → tool call)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (user → tool response)
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   []ContentBlock `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`

	// thinking (extended thinking)
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`                 // "base64" | "url"
	MediaType string `json:"media_type,omitempty"` // "image/jpeg" etc.
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

func NewTextMessage(role Role, text string) Message {
	return Message{
		Role:    role,
		Content: []ContentBlock{{Type: ContentTypeText, Text: text}},
	}
}

func (m Message) Text() string {
	var out string
	for _, b := range m.Content {
		if b.Type == ContentTypeText {
			out += b.Text
		}
	}
	return out
}

func (m Message) ToolUses() []ContentBlock {
	var uses []ContentBlock
	for _, b := range m.Content {
		if b.Type == ContentTypeToolUse {
			uses = append(uses, b)
		}
	}
	return uses
}

func (m Message) HasToolUse() bool {
	for _, b := range m.Content {
		if b.Type == ContentTypeToolUse {
			return true
		}
	}
	return false
}

type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_creation_input_tokens,omitempty"`
}

func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + other.InputTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
	}
}

type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonToolUse   StopReason = "tool_use"
	StopReasonMaxTokens StopReason = "max_tokens"
	StopReasonStopSeq   StopReason = "stop_sequence"
)

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ThinkingLevel string

const (
	ThinkingLevelOff     ThinkingLevel = "off"
	ThinkingLevelMinimal ThinkingLevel = "minimal"
	ThinkingLevelLow     ThinkingLevel = "low"
	ThinkingLevelMedium  ThinkingLevel = "medium"
	ThinkingLevelHigh    ThinkingLevel = "high"
	ThinkingLevelXHigh   ThinkingLevel = "xhigh"
)
