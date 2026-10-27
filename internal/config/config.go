// Package config loads converse-tui settings from a YAML file and environment
// variables. Precedence (lowest to highest): file < environment < CLI flags.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config holds all user-tunable settings. AWS credentials are never stored
// here; they come from the standard AWS credential chain (profile, env vars,
// SSO, instance role, ...).
type Config struct {
	// Profile is the AWS shared-config profile. Empty uses the SDK default
	// (AWS_PROFILE or "default").
	Profile string `yaml:"profile"`
	// Region is the AWS region. Empty uses the profile/AWS_REGION value.
	Region string `yaml:"region"`
	// ModelID is a Bedrock model ID or inference profile ID/ARN. Required.
	ModelID string `yaml:"model_id"`
	// SystemPrompt is sent with every request when non-empty.
	SystemPrompt string `yaml:"system_prompt"`
	// MaxTokens caps the response length. Zero uses the model default.
	MaxTokens int32 `yaml:"max_tokens"`
	// Temperature is only sent when set; some models reject it.
	Temperature *float32 `yaml:"temperature"`
	// Tools enables the local file tools. Defaults to true when unset.
	Tools *bool `yaml:"tools"`
	// Workdir confines file tools to this directory. Empty = current directory.
	Workdir string `yaml:"workdir"`
	// AutoApprove runs file-changing tools without asking.
	AutoApprove bool `yaml:"auto_approve"`
	// Mouse enables wheel scrolling. Defaults to true when unset. While on,
	// hold Shift to select text with the mouse.
	Mouse *bool `yaml:"mouse"`

	// Banner is the ASCII-art banner at the top of the chat.
	Banner Banner `yaml:"banner"`
	// AccentColor is a hex color ("#d97757") or ANSI color number ("208").
	AccentColor string `yaml:"accent_color"`
	// Theme is "dark" (default) or "light"; it picks the markdown and UI palette.
	Theme string `yaml:"theme"`
	// Markdown renders assistant replies as markdown. Defaults to true.
	Markdown *bool `yaml:"markdown"`
}

// Banner configures the welcome banner.
type Banner struct {
	// Show hides the banner when false. Defaults to true.
	Show *bool `yaml:"show"`
	// Text is rendered as ASCII art in Font. Default "converse".
	Text string `yaml:"text"`
	// Font is a FIGlet font name (standard, slant, small, big, doom, ...).
	Font string `yaml:"font"`
	// Art is literal ASCII art; when set it is used instead of Text/Font.
	Art string `yaml:"art"`
	// Title is the line under the banner. Default "Welcome to converse-tui".
	Title string `yaml:"title"`
}

// MarkdownEnabled reports whether replies are rendered as markdown.
func (c Config) MarkdownEnabled() bool { return c.Markdown == nil || *c.Markdown }

// MouseEnabled reports whether mouse wheel scrolling is on.
func (c Config) MouseEnabled() bool { return c.Mouse == nil || *c.Mouse }

// ToolsEnabled reports whether file tools are on.
func (c Config) ToolsEnabled() bool { return c.Tools == nil || *c.Tools }

// Environment variable names that override file values.
const (
	EnvConfig       = "CONVERSE_TUI_CONFIG"
	EnvProfile      = "CONVERSE_TUI_PROFILE"
	EnvRegion       = "CONVERSE_TUI_REGION"
	EnvModelID      = "CONVERSE_TUI_MODEL_ID"
	EnvSystemPrompt = "CONVERSE_TUI_SYSTEM_PROMPT"
	EnvMaxTokens    = "CONVERSE_TUI_MAX_TOKENS"
	EnvTemperature  = "CONVERSE_TUI_TEMPERATURE"
	EnvTools        = "CONVERSE_TUI_TOOLS"
	EnvWorkdir      = "CONVERSE_TUI_WORKDIR"
	EnvAutoApprove  = "CONVERSE_TUI_AUTO_APPROVE"
	EnvMouse        = "CONVERSE_TUI_MOUSE"
)

// DefaultPath returns the per-user config file location:
//
//	Linux:   $XDG_CONFIG_HOME/converse-tui/config.yaml (~/.config/...)
//	Windows: %AppData%\converse-tui\config.yaml
//	macOS:   ~/Library/Application Support/converse-tui/config.yaml
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "converse-tui", "config.yaml"), nil
}

// Load reads the config file at path (if it exists) and applies environment
// overrides. A missing file is not an error unless required is true.
func Load(path string, required bool) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !required:
		// fall through to env-only config
	default:
		return cfg, fmt.Errorf("read config: %w", err)
	}
	return cfg, cfg.applyEnv()
}

func (c *Config) applyEnv() error {
	if v, ok := os.LookupEnv(EnvProfile); ok {
		c.Profile = v
	}
	if v, ok := os.LookupEnv(EnvRegion); ok {
		c.Region = v
	}
	if v, ok := os.LookupEnv(EnvModelID); ok {
		c.ModelID = v
	}
	if v, ok := os.LookupEnv(EnvSystemPrompt); ok {
		c.SystemPrompt = v
	}
	if v, ok := os.LookupEnv(EnvMaxTokens); ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvMaxTokens, err)
		}
		c.MaxTokens = int32(n)
	}
	if v, ok := os.LookupEnv(EnvTemperature); ok && v != "" {
		f, err := strconv.ParseFloat(v, 32)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvTemperature, err)
		}
		t := float32(f)
		c.Temperature = &t
	}
	if v, ok := os.LookupEnv(EnvTools); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvTools, err)
		}
		c.Tools = &b
	}
	if v, ok := os.LookupEnv(EnvWorkdir); ok {
		c.Workdir = v
	}
	if v, ok := os.LookupEnv(EnvAutoApprove); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvAutoApprove, err)
		}
		c.AutoApprove = b
	}
	if v, ok := os.LookupEnv(EnvMouse); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvMouse, err)
		}
		c.Mouse = &b
	}
	return nil
}

// Validate reports settings that would make every request fail.
func (c Config) Validate() error {
	if c.ModelID == "" {
		return errors.New("no model configured: set model_id in the config file, " + EnvModelID + ", or -model (run with -init to create a config file)")
	}
	if c.Theme != "" && c.Theme != "dark" && c.Theme != "light" {
		return errors.New(`theme must be "dark" or "light"`)
	}
	if c.MaxTokens < 0 {
		return errors.New("max_tokens must be >= 0")
	}
	return nil
}

// WriteExample writes the example config to path, refusing to overwrite.
func WriteExample(path string, example []byte) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; not overwriting", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, example, 0o600)
}
