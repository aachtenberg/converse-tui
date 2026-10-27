// Package chat wraps the Amazon Bedrock Converse streaming API.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aachtenberg/converse-tui/internal/config"
	"github.com/aachtenberg/converse-tui/internal/tools"
)

// Client sends conversations to Bedrock.
type Client struct {
	rt     *bedrockruntime.Client
	cfg    config.Config
	region string
	tools  *tools.Runner // nil when tools are disabled
}

// New builds a client using the standard AWS credential chain. Pass a nil
// runner to disable tools.
func New(ctx context.Context, cfg config.Config, runner *tools.Runner) (*Client, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if awsCfg.Region == "" {
		return nil, errors.New("no AWS region: set region in the config file, AWS_REGION, or your AWS profile")
	}
	return &Client{rt: bedrockruntime.NewFromConfig(awsCfg), cfg: cfg, region: awsCfg.Region, tools: runner}, nil
}

// Region returns the resolved AWS region.
func (c *Client) Region() string { return c.region }

// ToolUse is a tool call requested by the model.
type ToolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// Event is one item from a streaming response. Text events carry a delta;
// the final event has Done set and carries the result.
type Event struct {
	Text string

	Done       bool
	Err        error
	StopReason string
	Usage      *types.TokenUsage
	// Message is the complete assistant turn (text, reasoning and tool-use
	// blocks) to append to the history. Nil if nothing was produced.
	Message  *types.Message
	ToolUses []ToolUse
}

// Stream sends the conversation and returns a channel of events. The channel
// is closed after the Done event. Cancel ctx to abort.
func (c *Client) Stream(ctx context.Context, msgs []types.Message) <-chan Event {
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		final := c.stream(ctx, msgs, ch)
		final.Done = true
		ch <- final
	}()
	return ch
}

func (c *Client) stream(ctx context.Context, msgs []types.Message, ch chan<- Event) Event {
	out, err := c.rt.ConverseStream(ctx, c.input(msgs))
	if err != nil {
		return Event{Err: err}
	}
	stream := out.GetStream()
	defer stream.Close()

	var final Event
	acc := newAccumulator()
	for ev := range stream.Events() {
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberContentBlockStart:
			if tu, ok := v.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
				b := acc.block(v.Value.ContentBlockIndex)
				b.toolID, b.toolName = aws.ToString(tu.Value.ToolUseId), aws.ToString(tu.Value.Name)
			}
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			b := acc.block(v.Value.ContentBlockIndex)
			switch d := v.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				b.text.WriteString(d.Value)
				select {
				case ch <- Event{Text: d.Value}:
				case <-ctx.Done():
					return Event{Err: ctx.Err()}
				}
			case *types.ContentBlockDeltaMemberToolUse:
				b.input.WriteString(aws.ToString(d.Value.Input))
			case *types.ContentBlockDeltaMemberReasoningContent:
				b.reasoning = true
				switch r := d.Value.(type) {
				case *types.ReasoningContentBlockDeltaMemberText:
					b.text.WriteString(r.Value)
				case *types.ReasoningContentBlockDeltaMemberSignature:
					b.signature += r.Value
				case *types.ReasoningContentBlockDeltaMemberRedactedContent:
					b.redacted = append(b.redacted, r.Value...)
				}
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			final.StopReason = string(v.Value.StopReason)
		case *types.ConverseStreamOutputMemberMetadata:
			final.Usage = v.Value.Usage
		}
	}
	if err := stream.Err(); err != nil {
		final.Err = err
		return final
	}
	if err := ctx.Err(); err != nil {
		final.Err = err
		return final
	}
	final.Message, final.ToolUses, final.Err = acc.message()
	return final
}

func (c *Client) input(msgs []types.Message) *bedrockruntime.ConverseStreamInput {
	input := &bedrockruntime.ConverseStreamInput{
		ModelId:  aws.String(c.cfg.ModelID),
		Messages: msgs,
	}
	if c.cfg.SystemPrompt != "" {
		input.System = append(input.System, &types.SystemContentBlockMemberText{Value: c.cfg.SystemPrompt})
	}
	if c.tools != nil {
		input.System = append(input.System, &types.SystemContentBlockMemberText{Value: fmt.Sprintf(
			"You can list, read, write and edit files under the working directory %s using the provided tools. "+
				"Paths are relative to it. Read a file before editing it.", c.tools.Root)})
		var specs []types.Tool
		for _, t := range c.tools.Tools() {
			specs = append(specs, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
				Name:        aws.String(t.Name),
				Description: aws.String(t.Description),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(t.Schema)},
			}})
		}
		input.ToolConfig = &types.ToolConfiguration{Tools: specs}
	}
	if c.cfg.MaxTokens > 0 || c.cfg.Temperature != nil {
		inf := &types.InferenceConfiguration{}
		if c.cfg.MaxTokens > 0 {
			inf.MaxTokens = aws.Int32(c.cfg.MaxTokens)
		}
		if c.cfg.Temperature != nil {
			inf.Temperature = aws.Float32(*c.cfg.Temperature)
		}
		input.InferenceConfig = inf
	}
	return input
}

// accumulator rebuilds content blocks from stream deltas, keyed by index.
type accumulator struct{ blocks map[int32]*block }

type block struct {
	text      strings.Builder
	reasoning bool
	signature string
	redacted  []byte
	toolID    string
	toolName  string
	input     strings.Builder
}

func newAccumulator() *accumulator { return &accumulator{blocks: map[int32]*block{}} }

func (a *accumulator) block(idx *int32) *block {
	i := aws.ToInt32(idx)
	b, ok := a.blocks[i]
	if !ok {
		b = &block{}
		a.blocks[i] = b
	}
	return b
}

func (a *accumulator) message() (*types.Message, []ToolUse, error) {
	idxs := make([]int32, 0, len(a.blocks))
	for i := range a.blocks {
		idxs = append(idxs, i)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })

	var content []types.ContentBlock
	var uses []ToolUse
	for _, i := range idxs {
		b := a.blocks[i]
		switch {
		case b.toolID != "":
			in := map[string]any{}
			if raw := b.input.String(); raw != "" {
				if err := json.Unmarshal([]byte(raw), &in); err != nil {
					return nil, nil, fmt.Errorf("invalid tool input for %s: %w", b.toolName, err)
				}
			}
			uses = append(uses, ToolUse{ID: b.toolID, Name: b.toolName, Input: in})
			content = append(content, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
				ToolUseId: aws.String(b.toolID),
				Name:      aws.String(b.toolName),
				Input:     document.NewLazyDocument(in),
			}})
		case b.reasoning && len(b.redacted) > 0:
			content = append(content, &types.ContentBlockMemberReasoningContent{
				Value: &types.ReasoningContentBlockMemberRedactedContent{Value: b.redacted},
			})
		case b.reasoning:
			// Reasoning blocks must be sent back unchanged during tool use.
			rt := types.ReasoningTextBlock{Text: aws.String(b.text.String())}
			if b.signature != "" {
				rt.Signature = aws.String(b.signature)
			}
			content = append(content, &types.ContentBlockMemberReasoningContent{
				Value: &types.ReasoningContentBlockMemberReasoningText{Value: rt},
			})
		case b.text.Len() > 0:
			content = append(content, &types.ContentBlockMemberText{Value: b.text.String()})
		}
	}
	if len(content) == 0 {
		return nil, nil, nil
	}
	return &types.Message{Role: types.ConversationRoleAssistant, Content: content}, uses, nil
}

// UserMessage builds a user turn.
func UserMessage(text string) types.Message {
	return types.Message{
		Role:    types.ConversationRoleUser,
		Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
	}
}

// ToolResult builds a tool result block.
func ToolResult(id, text string, isErr bool) types.ContentBlock {
	status := types.ToolResultStatusSuccess
	if isErr {
		status = types.ToolResultStatusError
	}
	return &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
		ToolUseId: aws.String(id),
		Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: text}},
		Status:    status,
	}}
}
