// Package bedrock implements the AWS Bedrock Converse Streaming API provider.
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brdoc "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/pi-agent/pi/internal/model"
	"github.com/pi-agent/pi/internal/provider"
)

// Provider implements provider.Provider for AWS Bedrock via the Converse Streaming API.
type Provider struct {
	client *bedrockruntime.Client
	region string
}

// New creates a new Bedrock provider, loading AWS credentials from the environment
// or ~/.aws/ credential/config files.
func New(region string) (*Provider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("bedrock: load AWS config: %w", err)
	}
	return &Provider{
		client: bedrockruntime.NewFromConfig(cfg),
		region: region,
	}, nil
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "bedrock" }

// ─── Model ID mapping ────────────────────────────────────────────────────────

// resolveModelID maps internal short names to Bedrock cross-region inference profile IDs.
// Falls through to the raw value for IDs already in Bedrock format.
func resolveModelID(m string) string {
	m = strings.TrimPrefix(m, "bedrock/")

	table := map[string]string{
		"claude-opus-4-7":   "us.anthropic.claude-opus-4-7-20250514-v1:0",
		"claude-sonnet-4-6": "us.anthropic.claude-sonnet-4-6-20241022-v2:0",
		"claude-haiku-4-5":  "us.anthropic.claude-haiku-4-5-20241022-v1:0",
		"claude-opus-4":     "us.anthropic.claude-opus-4-7-20250514-v1:0",
		"claude-sonnet-4":   "us.anthropic.claude-sonnet-4-6-20241022-v2:0",
		"claude-haiku-4":    "us.anthropic.claude-haiku-4-5-20241022-v1:0",
		"claude-3-opus":     "us.anthropic.claude-3-opus-20240229-v1:0",
		"claude-3-sonnet":   "us.anthropic.claude-3-sonnet-20240229-v1:0",
		"claude-3-haiku":    "us.anthropic.claude-3-haiku-20240307-v1:0",
		"claude-3-5-sonnet": "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		"claude-3-5-haiku":  "us.anthropic.claude-3-5-haiku-20241022-v1:0",
	}
	if id, ok := table[m]; ok {
		return id
	}
	return m
}

// ─── Request mapping ─────────────────────────────────────────────────────────

func mapMessages(msgs []model.Message) []types.Message {
	var out []types.Message
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			continue
		}
		role := types.ConversationRoleUser
		if m.Role == model.RoleAssistant {
			role = types.ConversationRoleAssistant
		}
		var blocks []types.ContentBlock
		for _, cb := range m.Content {
			if b := mapContentBlock(cb); b != nil {
				blocks = append(blocks, b)
			}
		}
		if len(blocks) == 0 {
			continue
		}
		out = append(out, types.Message{
			Role:    role,
			Content: blocks,
		})
	}
	return out
}

func mapContentBlock(cb model.ContentBlock) types.ContentBlock {
	switch cb.Type {
	case model.ContentTypeText:
		return &types.ContentBlockMemberText{Value: cb.Text}

	case model.ContentTypeImage:
		if cb.Source == nil {
			return nil
		}
		if cb.Source.Type == "base64" {
			return &types.ContentBlockMemberImage{
				Value: types.ImageBlock{
					Format: imageFormat(cb.Source.MediaType),
					Source: &types.ImageSourceMemberBytes{
						Value: []byte(cb.Source.Data),
					},
				},
			}
		}
		// Bedrock does not support URL images; fall back to a text description.
		return &types.ContentBlockMemberText{Value: cb.Source.URL}

	case model.ContentTypeToolUse:
		input := cb.Input
		if input == nil {
			input = json.RawMessage("{}")
		}
		var doc interface{}
		if err := json.Unmarshal(input, &doc); err != nil {
			doc = map[string]interface{}{}
		}
		return &types.ContentBlockMemberToolUse{
			Value: types.ToolUseBlock{
				ToolUseId: aws.String(cb.ID),
				Name:      aws.String(cb.Name),
				Input:     brdoc.NewLazyDocument(doc),
			},
		}

	case model.ContentTypeToolResult:
		var content []types.ToolResultContentBlock
		for _, c := range cb.Content {
			if c.Type == model.ContentTypeText {
				content = append(content, &types.ToolResultContentBlockMemberText{
					Value: c.Text,
				})
			}
		}
		if len(content) == 0 {
			text := cb.Text
			if text == "" {
				text = "{}"
			}
			content = append(content, &types.ToolResultContentBlockMemberText{Value: text})
		}
		status := types.ToolResultStatusSuccess
		if cb.IsError {
			status = types.ToolResultStatusError
		}
		return &types.ContentBlockMemberToolResult{
			Value: types.ToolResultBlock{
				ToolUseId: aws.String(cb.ToolUseID),
				Content:   content,
				Status:    status,
			},
		}

	default:
		if cb.Text != "" {
			return &types.ContentBlockMemberText{Value: cb.Text}
		}
		return nil
	}
}

func imageFormat(mediaType string) types.ImageFormat {
	switch mediaType {
	case "image/jpeg", "image/jpg":
		return types.ImageFormatJpeg
	case "image/png":
		return types.ImageFormatPng
	case "image/gif":
		return types.ImageFormatGif
	case "image/webp":
		return types.ImageFormatWebp
	default:
		return types.ImageFormatJpeg
	}
}

func mapSystem(msgs []model.Message, system string) []types.SystemContentBlock {
	var parts []string
	if system != "" {
		parts = append(parts, system)
	}
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			parts = append(parts, m.Text())
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []types.SystemContentBlock{
		&types.SystemContentBlockMemberText{
			Value: strings.Join(parts, "\n"),
		},
	}
}

func buildToolConfig(tools []model.ToolDefinition) *types.ToolConfiguration {
	if len(tools) == 0 {
		return nil
	}
	specs := make([]types.Tool, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schemaDoc interface{}
		if err := json.Unmarshal(schema, &schemaDoc); err != nil {
			schemaDoc = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		specs = append(specs, &types.ToolMemberToolSpec{
			Value: types.ToolSpecification{
				Name:        aws.String(t.Name),
				Description: aws.String(t.Description),
				InputSchema: &types.ToolInputSchemaMemberJson{
					Value: brdoc.NewLazyDocument(schemaDoc),
				},
			},
		})
	}
	return &types.ToolConfiguration{Tools: specs}
}

func mapStopReason(r types.StopReason) model.StopReason {
	switch r {
	case types.StopReasonEndTurn:
		return model.StopReasonEndTurn
	case types.StopReasonToolUse:
		return model.StopReasonToolUse
	case types.StopReasonMaxTokens:
		return model.StopReasonMaxTokens
	case types.StopReasonStopSequence:
		return model.StopReasonStopSeq
	default:
		return model.StopReasonEndTurn
	}
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a ConverseStream request to Bedrock and emits provider.Event values.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	mid := resolveModelID(req.Model)

	maxTokens := int32(req.MaxTokens)
	if maxTokens == 0 {
		maxTokens = 8096
	}

	inferConfig := &types.InferenceConfiguration{
		MaxTokens: aws.Int32(maxTokens),
	}
	if req.Temperature != nil {
		inferConfig.Temperature = aws.Float32(float32(*req.Temperature))
	}

	input := &bedrockruntime.ConverseStreamInput{
		ModelId:         aws.String(mid),
		Messages:        mapMessages(req.Messages),
		System:          mapSystem(req.Messages, req.System),
		InferenceConfig: inferConfig,
	}
	if len(req.Tools) > 0 {
		input.ToolConfig = buildToolConfig(req.Tools)
	}

	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		out, err := p.client.ConverseStream(ctx, input)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("bedrock: ConverseStream: %w", err)}
			return
		}

		stream := out.GetStream()
		defer stream.Close()

		// blockToTool maps Bedrock content-block index to our sequential tool index.
		blockToTool := map[int]int{}
		nextToolIdx := 0

		var inputTokens, outputTokens int32
		var stopReason types.StopReason

		for event := range stream.Events() {
			switch ev := event.(type) {

			case *types.ConverseStreamOutputMemberContentBlockStart:
				v := ev.Value
				switch bl := v.Start.(type) {
				case *types.ContentBlockStartMemberToolUse:
					id := ""
					name := ""
					if bl.Value.ToolUseId != nil {
						id = *bl.Value.ToolUseId
					}
					if bl.Value.Name != nil {
						name = *bl.Value.Name
					}
					myIdx := nextToolIdx
					nextToolIdx++
					blockToTool[int(aws.ToInt32(v.ContentBlockIndex))] = myIdx
					ch <- provider.Event{
						Type:      provider.EventToolCallStart,
						ToolID:    id,
						ToolName:  name,
						ToolIndex: myIdx,
					}
				}

			case *types.ConverseStreamOutputMemberContentBlockDelta:
				v := ev.Value
				switch d := v.Delta.(type) {
				case *types.ContentBlockDeltaMemberText:
					ch <- provider.Event{Type: provider.EventTextDelta, Text: d.Value}
				case *types.ContentBlockDeltaMemberToolUse:
					if toolIdx, ok := blockToTool[int(aws.ToInt32(v.ContentBlockIndex))]; ok {
						ch <- provider.Event{
							Type:        provider.EventToolCallDelta,
							ToolIndex:   toolIdx,
							PartialJSON: aws.ToString(d.Value.Input),
						}
					}
				}

			case *types.ConverseStreamOutputMemberContentBlockStop:
				v := ev.Value
				blockIdx := int(aws.ToInt32(v.ContentBlockIndex))
				if toolIdx, ok := blockToTool[blockIdx]; ok {
					ch <- provider.Event{
						Type:      provider.EventToolCallDone,
						ToolIndex: toolIdx,
					}
					delete(blockToTool, blockIdx)
				}

			case *types.ConverseStreamOutputMemberMessageStop:
				stopReason = ev.Value.StopReason

			case *types.ConverseStreamOutputMemberMetadata:
				if ev.Value.Usage != nil {
					if ev.Value.Usage.InputTokens != nil {
						inputTokens = *ev.Value.Usage.InputTokens
					}
					if ev.Value.Usage.OutputTokens != nil {
						outputTokens = *ev.Value.Usage.OutputTokens
					}
				}
			}
		}

		if err := stream.Err(); err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("bedrock: stream error: %w", err)}
			return
		}

		ch <- provider.Event{
			Type:       provider.EventMessageStop,
			StopReason: mapStopReason(stopReason),
			Usage: model.Usage{
				InputTokens:  int(inputTokens),
				OutputTokens: int(outputTokens),
			},
		}
	}()

	return ch, nil
}
