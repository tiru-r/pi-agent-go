package model

import "strings"

// Tier classifies a model by capability and cost level.
type Tier string

const (
	// TierToolless is for models that don't support function calling.
	TierToolless Tier = "toolless"
	// TierSmall is for cheap/free small models (≤ $0.20/1M input tokens).
	TierSmall Tier = "small"
	// TierMid is for mid-range models ($0.20–$1.00/1M input).
	TierMid Tier = "mid"
	// TierLarge is for capable models ($1–$5/1M input).
	TierLarge Tier = "large"
	// TierFrontier is for top-tier models (≥ $5/1M input).
	TierFrontier Tier = "frontier"
)

// Profile holds tier-derived harness defaults for a specific model.
// A zero-value Profile reproduces the original one-size-fits-all behavior.
type Profile struct {
	Tier Tier `json:"tier,omitempty"`

	// MaxOutputTokens caps the per-request output token count.
	// 0 means use the agent's configured default (DefaultMaxTokens).
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`

	// RecommendedMaxTurns is the default iteration cap for the agentic loop.
	// 0 means unlimited (Agent.Run) or the harness hard-coded default (SessionAgent).
	RecommendedMaxTurns int `json:"recommended_max_turns,omitempty"`

	// ParallelToolBudget is the max number of non-bash tool calls that may run
	// concurrently in a single turn. 0 means unlimited.
	ParallelToolBudget int `json:"parallel_tool_budget,omitempty"`

	// CompactionReserveRatio is the fraction of ContextWindow reserved for output
	// when calculating the compaction threshold.
	// 0 means use the compaction package default (8%).
	CompactionReserveRatio float64 `json:"compaction_reserve_ratio,omitempty"`

	// RetryAttempts is the max number of provider.Stream retry attempts.
	// 0 means use the retry package default (3).
	RetryAttempts int `json:"retry_attempts,omitempty"`

	// ToolChoiceTurn0 is the tool_choice value for the first turn of act/handoff
	// modes. "" defaults to "required" (existing behavior).
	ToolChoiceTurn0 string `json:"tool_choice_turn0,omitempty"`

	// TemperatureDefault is sent on every request when non-nil.
	// nil means leave temperature unset (let the model/provider decide).
	// Ignored when ThinkingLevel > Off (openrouter forces temperature=1.0).
	TemperatureDefault *float64 `json:"temperature_default,omitempty"`

	// RepairToolJSON enables lightweight JSON repair on malformed tool arguments
	// before returning the error to the model. Useful for small models that often
	// emit trailing commas, single quotes, or bare strings.
	RepairToolJSON bool `json:"repair_tool_json,omitempty"`
}

// ApplyMaxTokens returns the effective max-output-tokens for a request.
//
// The profile's MaxOutputTokens is a tier-appropriate sensible default —
// it is applied only when the caller is using the system-wide default
// (current == systemDefault or current <= 0). When the user has explicitly
// configured a value different from the system default, that value is honored
// and the profile does not cap it downward.
//
//   current == systemDefault → profile default (more conservative for small
//                              models, more generous for frontier models)
//   current != systemDefault → current (user's explicit choice is respected)
//   current <= 0             → profile default, or systemDefault if no profile
func (p Profile) ApplyMaxTokens(current, systemDefault int) int {
	if p.MaxOutputTokens <= 0 {
		if current <= 0 {
			return systemDefault
		}
		return current
	}
	if current <= 0 || current == systemDefault {
		return p.MaxOutputTokens
	}
	return current
}

// MaxTurnsOr resolves the effective turn cap:
//
//	explicit > 0  → explicit (caller's hard limit wins over profile)
//	explicit == -1 → 0 (explicitly unlimited; bypasses the profile cap)
//	explicit == 0  → p.RecommendedMaxTurns (profile default; 0 = unlimited)
func (p Profile) MaxTurnsOr(explicit int) int {
	if explicit < 0 {
		return 0 // -1 sentinel: explicitly unlimited
	}
	if explicit > 0 {
		return explicit
	}
	return p.RecommendedMaxTurns
}

// ToolChoiceOrDefault returns p.ToolChoiceTurn0, falling back to "required"
// (the original harness default) when the profile is unset.
func (p Profile) ToolChoiceOrDefault() string {
	if p.ToolChoiceTurn0 != "" {
		return p.ToolChoiceTurn0
	}
	return "required"
}

// RetryAttemptsOr returns explicit when set (>0), otherwise p.RetryAttempts.
func (p Profile) RetryAttemptsOr(explicit int) int {
	if explicit > 0 {
		return explicit
	}
	return p.RetryAttempts
}

// TemperatureFor returns the effective temperature for a request.
// configTemp is the user's explicit setting (from config file / env) and takes
// priority over the tier-based p.TemperatureDefault when non-nil.
// Both are ignored when thinking is enabled (the provider forces temperature=1.0).
func (p Profile) TemperatureFor(level ThinkingLevel, configTemp *float64) *float64 {
	if level != ThinkingLevelOff {
		return nil
	}
	if configTemp != nil {
		return configTemp
	}
	return p.TemperatureDefault
}

// ClassifyModel derives a Profile from m using a layered heuristic.
// overrides maps model IDs to user-pinned Profiles; a matching entry
// short-circuits all other classification logic. Pass nil for no overrides.
func ClassifyModel(m ModelInfo, overrides map[string]Profile) Profile {
	if p, ok := overrides[m.ID]; ok {
		return p
	}

	// Models that can't call tools: single-turn, no-tool profile.
	if !m.SupportsTools {
		return Profile{
			Tier:                TierToolless,
			MaxOutputTokens:     4096,
			RecommendedMaxTurns: 1,
		}
	}

	tier := pricingTier(m)

	// Free-tier cap: IDs suffixed with ":free" are often rate-limited or smaller
	// variants — cap at TierMid regardless of how their pricing is listed.
	if (tier == TierLarge || tier == TierFrontier) && strings.HasSuffix(m.ID, ":free") {
		tier = TierMid
	}

	// Dimension bumps: when pricing metadata is missing or small, use model
	// dimensions as additional signals. Both checks only promote, never demote.
	//
	// Context-window bump (Small → Mid): a large context window suggests a
	// capable model. Threshold 200k is well above commodity 128k models.
	if tier == TierSmall && m.ContextWindow >= 200_000 {
		tier = TierMid
	}
	// Max-output bump (Small/Mid → Large): a high output ceiling is a strong
	// capability signal. OpenRouter uses 4096 as a fallback for unknown models,
	// so only act when MaxTokens is explicitly above that baseline.
	if m.MaxTokens > 32_768 && (tier == TierSmall || tier == TierMid) {
		tier = TierLarge
	}

	p := profileForTier(tier)

	// Clamp MaxOutputTokens to the model's actual ceiling when it is explicitly
	// known (> the 4096 fallback). This prevents API errors when a high-priced
	// but output-limited model gets a tier whose default exceeds its real cap.
	if m.MaxTokens > 4096 && m.MaxTokens < p.MaxOutputTokens {
		p.MaxOutputTokens = m.MaxTokens
	}

	return p
}

// pricingTier classifies a model by its OpenRouter cost metadata.
func pricingTier(m ModelInfo) Tier {
	switch {
	case m.InputCostPer1M >= 5.0 || m.OutputCostPer1M >= 25.0:
		return TierFrontier
	case m.InputCostPer1M >= 1.0 || m.OutputCostPer1M >= 5.0:
		return TierLarge
	case m.InputCostPer1M >= 0.20 || m.OutputCostPer1M >= 1.0:
		return TierMid
	default:
		return TierSmall
	}
}

func profileForTier(tier Tier) Profile {
	switch tier {
	case TierSmall:
		t := 0.3
		return Profile{
			Tier:                   TierSmall,
			MaxOutputTokens:        2048,
			RecommendedMaxTurns:    20, // preserves original defaultMaxIter baseline
			ParallelToolBudget:     1,
			CompactionReserveRatio: 0.15,
			RetryAttempts:          5,
			ToolChoiceTurn0:        "auto",
			TemperatureDefault:     &t,
			RepairToolJSON:         true,
		}
	case TierMid:
		return Profile{
			Tier:                   TierMid,
			MaxOutputTokens:        4096,
			RecommendedMaxTurns:    30,
			ParallelToolBudget:     2,
			CompactionReserveRatio: 0.10,
			RetryAttempts:          3,
			ToolChoiceTurn0:        "auto",
			RepairToolJSON:         true,
		}
	case TierLarge:
		return Profile{
			Tier:                   TierLarge,
			MaxOutputTokens:        8192,
			RecommendedMaxTurns:    40,
			ParallelToolBudget:     4,
			CompactionReserveRatio: 0.08,
			RetryAttempts:          3,
			ToolChoiceTurn0:        "required",
		}
	case TierFrontier:
		return Profile{
			Tier:                   TierFrontier,
			MaxOutputTokens:        16384,
			RecommendedMaxTurns:    60,
			ParallelToolBudget:     8,
			CompactionReserveRatio: 0.06,
			RetryAttempts:          2,
			ToolChoiceTurn0:        "required",
		}
	default:
		return Profile{}
	}
}
