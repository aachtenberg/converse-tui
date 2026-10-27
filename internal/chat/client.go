// Package chat wraps the Amazon Bedrock Converse streaming API.
package chat

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aachtenberg/converse-tui/internal/config"
)

// Client sends conversations to Bedrock.
type Client struct {
	rt     *bedrockruntime.Client
	cfg    config.Config
	region string
}

// New builds a client using the standard AWS credential chain.
func New(ctx context.Context, cfg config.Config) (*Client, error) {
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
	return &Client{rt: bedrockruntime.NewFromConfig(awsCfg), cfg: cfg, region: awsCfg.Region}, nil
}

// Region returns the resolved AWS region.
func (c *Client) Region() string { return c.region }

// Event is one item from a streaming response. Text events carry a delta;
// the final event has Done set and may carry Err, StopReason and Usage.
type Event struct {
	Text       string
	Done       bool
	Err        error
	StopReason string
	Usage      *types.TokenUsage
}

// Stream sends the conversation and returns a channel of events. The channel
// is closed after the Done event. Cancel ctx to abort.
func (c *Client) Stream(ctx context.Context, msgs []types.Message) <-chan Event {
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		stop, usage, err := c.stream(ctx, msgs, ch)
		ch <- Event{Done: true, Err: err, StopReason: stop, Usage: usage}
	}()
	return ch
}

func (c *Client) stream(ctx context.Context, msgs []types.Message, ch chan<- Event) (string, *types.TokenUsage, error) {
	input := &bedrockruntime.ConverseStreamInput{
		ModelId:  aws.String(c.cfg.ModelID),
		Messages: msgs,
	}
	if c.cfg.SystemPrompt != "" {
		input.System = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: c.cfg.SystemPrompt},
		}
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

	out, err := c.rt.ConverseStream(ctx, input)
	if err != nil {
		return "", nil, err
	}
	stream := out.GetStream()
	defer stream.Close()

	var stop string
	var usage *types.TokenUsage
	for ev := range stream.Events() {
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			if d, ok := v.Value.Delta.(*types.ContentBlockDeltaMemberText); ok {
				select {
				case ch <- Event{Text: d.Value}:
				case <-ctx.Done():
					return stop, usage, ctx.Err()
				}
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			stop = string(v.Value.StopReason)
		case *types.ConverseStreamOutputMemberMetadata:
			usage = v.Value.Usage
		}
	}
	if err := stream.Err(); err != nil {
		return stop, usage, err
	}
	return stop, usage, ctx.Err()
}

// UserMessage builds a user turn.
func UserMessage(text string) types.Message {
	return types.Message{
		Role:    types.ConversationRoleUser,
		Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
	}
}

// AssistantMessage builds an assistant turn.
func AssistantMessage(text string) types.Message {
	return types.Message{
		Role:    types.ConversationRoleAssistant,
		Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
	}
}
