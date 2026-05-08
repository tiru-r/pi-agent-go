// Package cli defines the cobra command tree for the pi binary.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/pi-agent/pi/internal/acp"
	"github.com/pi-agent/pi/internal/agent"
	"github.com/pi-agent/pi/internal/config"
	"github.com/pi-agent/pi/internal/model"
	"github.com/pi-agent/pi/internal/provider/anthropic"
	"github.com/pi-agent/pi/internal/session"
	"github.com/pi-agent/pi/internal/tui"
)

// Version is injected at build time via -ldflags.
var Version = "0.1.0"

// globalFlags holds values parsed from root-level persistent flags.
type globalFlags struct {
	model     string
	provider  string
	system    string
	sessionID string
	noSession bool
	think     bool
	debug     bool
}

// NewRootCmd builds and returns the root cobra command.
func NewRootCmd() *cobra.Command {
	var gf globalFlags

	root := &cobra.Command{
		Use:   "pi",
		Short: "Pi — AI coding agent",
		Long: `Pi is an interactive AI coding agent powered by large language models.

Run without arguments to start the interactive TUI.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInteractiveTUI(&gf)
		},
	}

	// Persistent flags available to all sub-commands.
	pf := root.PersistentFlags()
	pf.StringVarP(&gf.model, "model", "m", "", "Override LLM model")
	pf.StringVarP(&gf.provider, "provider", "p", "", "Override provider (anthropic, openai, …)")
	pf.StringVar(&gf.system, "system", "", "Override system prompt")
	pf.StringVarP(&gf.sessionID, "session", "s", "", "Session ID to continue")
	pf.BoolVar(&gf.noSession, "no-session", false, "Disable session persistence")
	pf.BoolVar(&gf.think, "think", false, "Enable extended thinking")
	pf.BoolVar(&gf.debug, "debug", false, "Enable debug logging")

	// --acp runs the Zed Agent Client Protocol server over stdio instead of the TUI.
	var acpMode bool
	root.Flags().BoolVar(&acpMode, "acp", false, "Run as Zed ACP language model server (stdio)")
	root.PreRunE = func(cmd *cobra.Command, args []string) error {
		if acpMode {
			return runACPServer(&gf)
		}
		return nil
	}

	// Sub-commands.
	root.AddCommand(
		newRunCmd(&gf),
		newSessionCmd(&gf),
		newAuthCmd(&gf),
		newModelsCmd(&gf),
		newDoctorCmd(&gf),
		newConfigCmd(&gf),
		newVersionCmd(),
		newACPCmd(&gf),
	)

	return root
}

// ── Shared setup ──────────────────────────────────────────────────────────────

// loadConfig loads config and applies global flag overrides.
func loadConfig(gf *globalFlags) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if gf.model != "" {
		cfg.Model = gf.model
	}
	if gf.provider != "" {
		cfg.Provider = gf.provider
	}
	if gf.system != "" {
		cfg.SystemPrompt = gf.system
	}
	if gf.think {
		cfg.ThinkingLevel = string(model.ThinkingLevelFull)
	}
	return cfg, nil
}

// buildAgent creates the agent for the given config.
func buildAgent(cfg *config.Config) (*agent.Agent, error) {
	prov, err := buildProvider(cfg)
	if err != nil {
		return nil, err
	}
	return agent.New(prov, cfg.Model, cfg.SystemPrompt, cfg.MaxTokens), nil
}

// buildProvider constructs the provider from config.
func buildProvider(cfg *config.Config) (*anthropic.Provider, error) {
	switch cfg.Provider {
	case "anthropic", "":
		if cfg.AnthropicAPIKey == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set; run: pi auth set anthropic <key>")
		}
		return anthropic.New(cfg.AnthropicAPIKey), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q — only 'anthropic' is built in; set ANTHROPIC_API_KEY", cfg.Provider)
	}
}

// openOrNewSession returns a session, or nil when no-session is set.
func openOrNewSession(cfg *config.Config, gf *globalFlags) (*session.Session, error) {
	if gf.noSession {
		return nil, nil
	}
	if gf.sessionID != "" {
		path := cfg.SessionDir + "/" + gf.sessionID + ".jsonl"
		return session.Open(path)
	}
	return session.New(cfg.SessionDir)
}

// ── pi (root — interactive TUI) ───────────────────────────────────────────────

func runInteractiveTUI(gf *globalFlags) error {
	cfg, err := loadConfig(gf)
	if err != nil {
		return err
	}
	ag, err := buildAgent(cfg)
	if err != nil {
		return err
	}
	sess, err := openOrNewSession(cfg, gf)
	if err != nil {
		return err
	}

	m := tui.New(cfg, ag, sess)
	return tui.Run(m)
}

// ── pi run ────────────────────────────────────────────────────────────────────

func newRunCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "run <prompt>",
		Short: "Run agent with a single prompt and stream output to stdout",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := strings.Join(args, " ")
			cfg, err := loadConfig(gf)
			if err != nil {
				return err
			}
			ag, err := buildAgent(cfg)
			if err != nil {
				return err
			}
			sess, err := openOrNewSession(cfg, gf)
			if err != nil {
				return err
			}

			var history []model.Message
			if sess != nil {
				history = sess.Messages()
			}

			opts := agent.Options{
				System:        cfg.SystemPrompt,
				ThinkingLevel: model.ThinkingLevel(cfg.ThinkingLevel),
			}

			_, err = ag.Run(context.Background(), prompt, history, opts,
				func(ev agent.AgentEvent) {
					switch ev.Kind {
					case agent.EventKindText:
						fmt.Print(ev.Delta)
					case agent.EventKindThinking:
						// skip thinking in one-shot mode
					case agent.EventKindToolStart:
						fmt.Fprintf(os.Stderr, "\n[tool: %s]\n", ev.ToolName)
					case agent.EventKindDone:
						fmt.Println()
					case agent.EventKindError:
						fmt.Fprintf(os.Stderr, "\nerror: %v\n", ev.Err)
					}
				})
			return err
		},
	}
}

// ── pi session ────────────────────────────────────────────────────────────────

func newSessionCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Session management",
	}

	cmd.AddCommand(
		newSessionListCmd(gf),
		newSessionOpenCmd(gf),
		newSessionShowCmd(gf),
		newSessionDeleteCmd(gf),
	)
	return cmd
}

func newSessionListCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(gf)
			if err != nil {
				return err
			}

			store, err := session.NewSQLiteStore(cfg.SessionDir + "/index.db")
			if err != nil {
				// Fallback: enumerate JSONL files.
				return listSessionsJSONL(cfg.SessionDir)
			}
			defer store.Close()

			metas, err := store.ListSessions()
			if err != nil {
				return err
			}
			if len(metas) == 0 {
				fmt.Println("No sessions found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\tTITLE\tMODEL\tMSGS\tUPDATED\n")
			for _, m := range metas {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
					m.ID[:8],
					truncateStr(m.Title, 40),
					m.Model,
					m.MessageCount,
					m.UpdatedAt.Format("2006-01-02 15:04"),
				)
			}
			return w.Flush()
		},
	}
}

func listSessionsJSONL(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read session dir: %w", err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		sess, err := session.Open(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		fmt.Printf("%s  %s\n", id[:8], sess.Title())
		found++
	}
	if found == 0 {
		fmt.Println("No sessions found.")
	}
	return nil
}

func newSessionOpenCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "open <id>",
		Short: "Open a session in the TUI",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			gf.sessionID = args[0]
			return runInteractiveTUI(gf)
		},
	}
}

func newSessionShowCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Print session conversation to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(gf)
			if err != nil {
				return err
			}
			id := args[0]
			path := cfg.SessionDir + "/" + id + ".jsonl"
			sess, err := session.Open(path)
			if err != nil {
				return fmt.Errorf("open session: %w", err)
			}
			msgs := sess.Messages()
			for _, msg := range msgs {
				role := string(msg.Role)
				fmt.Printf("[%s]\n%s\n\n", strings.ToUpper(role), msg.Text())
			}
			return nil
		},
	}
}

func newSessionDeleteCmd(gf *globalFlags) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(gf)
			if err != nil {
				return err
			}
			id := args[0]
			if !force {
				fmt.Printf("Delete session %s? [y/N] ", id)
				reader := bufio.NewReader(os.Stdin)
				answer, _ := reader.ReadString('\n')
				answer = strings.TrimSpace(strings.ToLower(answer))
				if answer != "y" && answer != "yes" {
					fmt.Println("Aborted.")
					return nil
				}
			}
			path := cfg.SessionDir + "/" + id + ".jsonl"
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("delete session: %w", err)
			}
			fmt.Printf("Session %s deleted.\n", id)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmation")
	return cmd
}

// ── pi auth ───────────────────────────────────────────────────────────────────

func newAuthCmd(_ *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "API key management",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "set <provider> <key>",
			Short: "Store an API key",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				prov, key := args[0], args[1]
				switch strings.ToLower(prov) {
				case "anthropic":
					cfg.AnthropicAPIKey = key
				case "openai":
					cfg.OpenAIAPIKey = key
				case "gemini", "google":
					cfg.GeminiAPIKey = key
				case "cohere":
					cfg.CohereAPIKey = key
				case "azure":
					cfg.AzureAPIKey = key
				default:
					return fmt.Errorf("unknown provider %q", prov)
				}
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Printf("API key for %s saved.\n", prov)
				return nil
			},
		},
		&cobra.Command{
			Use:   "check",
			Short: "Validate all configured API keys",
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				type check struct {
					name string
					key  string
				}
				checks := []check{
					{"anthropic", cfg.AnthropicAPIKey},
					{"openai", cfg.OpenAIAPIKey},
					{"gemini", cfg.GeminiAPIKey},
					{"cohere", cfg.CohereAPIKey},
					{"azure", cfg.AzureAPIKey},
				}
				for _, c := range checks {
					if c.key != "" {
						fmt.Printf("  %-12s  configured\n", c.name)
					}
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show which providers have API keys configured",
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				printKeyStatus := func(name, key string) {
					if key != "" {
						masked := key
						if len(key) > 8 {
							masked = key[:4] + "****" + key[len(key)-4:]
						}
						fmt.Printf("  %-12s  %s\n", name, masked)
					} else {
						fmt.Printf("  %-12s  (not set)\n", name)
					}
				}
				fmt.Println("API key status:")
				printKeyStatus("anthropic", cfg.AnthropicAPIKey)
				printKeyStatus("openai", cfg.OpenAIAPIKey)
				printKeyStatus("gemini", cfg.GeminiAPIKey)
				printKeyStatus("cohere", cfg.CohereAPIKey)
				printKeyStatus("azure", cfg.AzureAPIKey)
				return nil
			},
		},
	)
	return cmd
}

// ── pi models ─────────────────────────────────────────────────────────────────

func newModelsCmd(_ *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List available models",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\tPROVIDER\tDISPLAY NAME\tMAX TOKENS\tFEATURES\n")
			for _, m := range model.Registry {
				var features []string
				if m.SupportsTools {
					features = append(features, "tools")
				}
				if m.SupportsVision {
					features = append(features, "vision")
				}
				if m.SupportsThinking {
					features = append(features, "thinking")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
					m.ID, m.Provider, m.DisplayName, m.MaxTokens,
					strings.Join(features, ","))
			}
			return w.Flush()
		},
	}
}

// ── pi doctor ─────────────────────────────────────────────────────────────────

func newDoctorCmd(_ *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Pi doctor")
			fmt.Println()

			ok := true
			cfg, err := config.Load()

			// Config load
			if err != nil {
				fmt.Printf("  [FAIL] load config: %v\n", err)
				ok = false
			} else {
				fmt.Println("  [ OK ] config loaded")

				// Session dir
				if err := os.MkdirAll(cfg.SessionDir, 0o700); err != nil {
					fmt.Printf("  [FAIL] session dir: %v\n", err)
					ok = false
				} else {
					fmt.Printf("  [ OK ] session dir: %s\n", cfg.SessionDir)
				}

				// API key presence
				if cfg.AnthropicAPIKey != "" {
					fmt.Println("  [ OK ] ANTHROPIC_API_KEY set")
				} else {
					fmt.Println("  [WARN] ANTHROPIC_API_KEY not set")
				}

				// Model
				if _, found := model.Lookup(cfg.Model); found {
					fmt.Printf("  [ OK ] model %q is in registry\n", cfg.Model)
				} else {
					fmt.Printf("  [WARN] model %q is not in registry (may still work)\n", cfg.Model)
				}
			}

			if !ok {
				fmt.Println("\nSome checks failed.")
				os.Exit(1)
			}
			fmt.Println("\nAll checks passed.")
			return nil
		},
	}
}

// ── pi config ────────────────────────────────────────────────────────────────

func newConfigCmd(_ *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or edit configuration",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print current configuration",
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintf(w, "provider\t%s\n", cfg.Provider)
				fmt.Fprintf(w, "model\t%s\n", cfg.Model)
				fmt.Fprintf(w, "max_tokens\t%d\n", cfg.MaxTokens)
				fmt.Fprintf(w, "thinking_level\t%s\n", cfg.ThinkingLevel)
				fmt.Fprintf(w, "theme\t%s\n", cfg.Theme)
				fmt.Fprintf(w, "session_dir\t%s\n", cfg.SessionDir)
				return w.Flush()
			},
		},
		&cobra.Command{
			Use:   "set <key> <value>",
			Short: "Update a configuration setting",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				key, val := args[0], args[1]
				switch key {
				case "provider":
					cfg.Provider = val
				case "model":
					cfg.Model = val
				case "theme":
					cfg.Theme = val
				case "thinking_level":
					cfg.ThinkingLevel = val
				case "system_prompt":
					cfg.SystemPrompt = val
				default:
					return fmt.Errorf("unknown config key %q", key)
				}
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Printf("Set %s = %s\n", key, val)
				return nil
			},
		},
	)
	return cmd
}

// ── pi version ───────────────────────────────────────────────────────────────

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("pi version %s\n", Version)
		},
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// ── pi acp ───────────────────────────────────────────────────────────────────

// newACPCmd returns the explicit `pi acp` subcommand (alternative to --acp flag).
func newACPCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "acp",
		Short: "Run as Zed ACP language model server (stdio)",
		Long: `Start pi as a Zed Agent Client Protocol (ACP) server.

Zed invokes this as a subprocess and communicates over stdin/stdout using
line-delimited JSON-RPC 2.0.  You do not normally call this yourself.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runACPServer(gf)
		},
	}
}

// runACPServer starts the ACP server and blocks until stdin closes.
func runACPServer(gf *globalFlags) error {
	cfg, err := loadConfig(gf)
	if err != nil {
		return err
	}
	srv, err := acp.New(cfg)
	if err != nil {
		return fmt.Errorf("acp: %w", err)
	}
	return srv.Serve(context.Background())
}
