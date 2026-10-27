// Command converse-tui is a terminal chat client for the Amazon Bedrock
// Converse API.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aachtenberg/converse-tui/internal/chat"
	"github.com/aachtenberg/converse-tui/internal/config"
	"github.com/aachtenberg/converse-tui/internal/tools"
	"github.com/aachtenberg/converse-tui/internal/ui"
)

//go:embed config.example.yaml
var exampleConfig []byte

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	pathFromEnv := os.Getenv(config.EnvConfig)
	if pathFromEnv != "" {
		defaultPath = pathFromEnv
	}

	configPath := flag.String("config", defaultPath, "path to config file (env "+config.EnvConfig+")")
	profile := flag.String("profile", "", "AWS profile (overrides config)")
	region := flag.String("region", "", "AWS region (overrides config)")
	model := flag.String("model", "", "Bedrock model ID or inference profile (overrides config)")
	workdir := flag.String("workdir", "", "directory file tools may access (default: current directory)")
	noTools := flag.Bool("no-tools", false, "disable file tools")
	auto := flag.Bool("auto", false, "auto-approve file edits (toggle in-app with Ctrl+T)")
	initCfg := flag.Bool("init", false, "write an example config file to -config path and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("converse-tui", version)
		return nil
	}
	if *initCfg {
		if err := config.WriteExample(*configPath, exampleConfig); err != nil {
			return err
		}
		fmt.Println("wrote", *configPath, "- edit model_id, then run converse-tui")
		return nil
	}

	explicit := pathFromEnv != ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	cfg, err := config.Load(*configPath, explicit)
	if err != nil {
		return err
	}
	if *profile != "" {
		cfg.Profile = *profile
	}
	if *region != "" {
		cfg.Region = *region
	}
	if *model != "" {
		cfg.ModelID = *model
	}
	if *workdir != "" {
		cfg.Workdir = *workdir
	}
	if *noTools {
		off := false
		cfg.Tools = &off
	}
	if *auto {
		cfg.AutoApprove = true
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	var runner *tools.Runner
	if cfg.ToolsEnabled() {
		root := cfg.Workdir
		if root == "" {
			root = "."
		}
		if runner, err = tools.New(root); err != nil {
			return err
		}
	}

	client, err := chat.New(context.Background(), cfg, runner)
	if err != nil {
		return err
	}

	banner, err := ui.RenderBanner(cfg.Banner)
	if err != nil {
		return err
	}

	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if cfg.MouseEnabled() {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	_, err = tea.NewProgram(ui.New(client, runner, ui.Options{Config: cfg, Banner: banner}), opts...).Run()
	return err
}
