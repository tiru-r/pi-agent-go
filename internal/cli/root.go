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

	"github.com/tiru-r/pi-agent-go/internal/acp"
	"github.com/tiru-r/pi-agent-go/internal/agent"
	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/factory"
	"github.com/tiru-r/pi-agent-go/internal/provider/openrouter"
	"github.com/tiru-r/pi-agent-go/internal/session"
)

// Version is injected at build time via -ldflags.
var Version = "0.1.0"

// globalFlags holds values parsed from root-level persistent flags.
type globalFlags struct {
	model     string
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
		Use:          "pi",
		Short:        "Pi — Zed native AI agent (OpenRouter)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&gf.model, "model", "m", "", "OpenRouter model ID override")
	pf.StringVar(&gf.system, "system", "", "Override system prompt")
	pf.StringVarP(&gf.sessionID, "session", "s", "", "Session ID to continue")
	pf.BoolVar(&gf.noSession, "no-session", false, "Disable session persistence")
	pf.BoolVar(&gf.think, "think", false, "Enable extended thinking")
	pf.BoolVar(&gf.debug, "debug", false, "Enable debug logging")

	root.AddCommand(
		newRunCmd(&gf),
		newSessionCmd(&gf),
		newAuthCmd(),
		newModelsCmd(),
		newDoctorCmd(),
		newConfigCmd(&gf),
		newVersionCmd(),
		newACPCmd(&gf),
	)

	return root
}

// ── Shared setup ──────────────────────────────────────────────────────────────

func loadConfig(gf *globalFlags) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if gf.model != "" {
		cfg.Model = gf.model
	}
	if gf.system != "" {
		cfg.SystemPrompt = gf.system
	}
	if gf.think {
		cfg.ThinkingLevel = string(model.ThinkingLevelFull)
	}
	return cfg, nil
}

func buildProvider(cfg *config.Config) (provider.Provider, error) {
	return factory.New(cfg)
}

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

// ── pi run ────────────────────────────────────────────────────────────────────

func newRunCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "run <prompt>",
		Short: "Run agent with a single prompt, stream to stdout",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := strings.Join(args, " ")
			cfg, err := loadConfig(gf)
			if err != nil {
				return err
			}
			prov, err := buildProvider(cfg)
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

			ag := agent.New(prov, cfg.Model, cfg.SystemPrompt, cfg.MaxTokens)
			opts := agent.Options{
				System:        cfg.SystemPrompt,
				ThinkingLevel: model.ThinkingLevel(cfg.ThinkingLevel),
			}
			_, err = ag.Run(context.Background(), prompt, history, opts,
				func(ev agent.AgentEvent) {
					switch ev.Kind {
					case agent.EventKindText:
						fmt.Print(ev.Delta)
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
		title := sess.Title()
		_ = sess.Close()
		fmt.Printf("%s  %s\n", id[:8], title)
		found++
	}
	if found == 0 {
		fmt.Println("No sessions found.")
	}
	return nil
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
			path := cfg.SessionDir + "/" + args[0] + ".jsonl"
			sess, err := session.Open(path)
			if err != nil {
				return fmt.Errorf("open session: %w", err)
			}
			defer sess.Close()
			for _, msg := range sess.Messages() {
				fmt.Printf("[%s]\n%s\n\n", strings.ToUpper(string(msg.Role)), msg.Text())
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

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "OpenRouter API key management",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "set <key>",
			Short: "Store the OpenRouter API key",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				cfg.OpenRouterAPIKey = args[0]
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Println("OpenRouter API key saved.")
				return nil
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show API key status",
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				if cfg.OpenRouterAPIKey == "" {
					fmt.Println("  openrouter  (not set)")
				} else {
					key := cfg.OpenRouterAPIKey
					masked := key
					if len(key) > 8 {
						masked = key[:4] + "****" + key[len(key)-4:]
					}
					fmt.Printf("  openrouter  %s\n", masked)
				}
				return nil
			},
		},
	)
	return cmd
}

// ── pi models ─────────────────────────────────────────────────────────────────

func newModelsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List available models from OpenRouter",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Fetching models from OpenRouter...\n")
			models, err := openrouter.FetchModels(cmd.Context(), cfg.OpenRouterAPIKey)
			if err != nil {
				return fmt.Errorf("fetch models: %w", err)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "ID\tNAME\tMAX TOKENS\tTOOLS\tVISION\n")
			for _, m := range models {
				tools := ""
				if m.SupportsTools {
					tools = "yes"
				}
				vision := ""
				if m.SupportsVision {
					vision = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
					m.ID, m.DisplayName, m.MaxTokens, tools, vision)
			}
			return w.Flush()
		},
	}
}

// ── pi doctor ─────────────────────────────────────────────────────────────────

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Pi doctor")
			fmt.Println()

			cfg, err := config.Load()
			if err != nil {
				fmt.Printf("  [FAIL] load config: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("  [ OK ] config loaded")

			if err := os.MkdirAll(cfg.SessionDir, 0o700); err != nil {
				fmt.Printf("  [FAIL] session dir: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("  [ OK ] session dir: %s\n", cfg.SessionDir)

			if cfg.OpenRouterAPIKey != "" {
				fmt.Println("  [ OK ] OPENROUTER_API_KEY set")
			} else {
				fmt.Println("  [WARN] OPENROUTER_API_KEY not set")
			}

			fmt.Printf("  [ OK ] model: %s\n", cfg.Model)
			fmt.Println("\nAll checks passed.")
			return nil
		},
	}
}

// ── pi config ────────────────────────────────────────────────────────────────

func newConfigCmd(gf *globalFlags) *cobra.Command {
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
				fmt.Fprintf(w, "model\t%s\n", cfg.Model)
				fmt.Fprintf(w, "max_tokens\t%d\n", cfg.MaxTokens)
				fmt.Fprintf(w, "thinking_level\t%s\n", cfg.ThinkingLevel)
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
				case "model":
					cfg.Model = val
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

// ── pi acp ───────────────────────────────────────────────────────────────────

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

// ── helpers ───────────────────────────────────────────────────────────────────

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
