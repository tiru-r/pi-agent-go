package model

import "testing"

func TestClassifyModel(t *testing.T) {
	tests := []struct {
		name      string
		info      ModelInfo
		wantTier  Tier
		overrides map[string]Profile
	}{
		// ── override short-circuits everything ────────────────────────────
		{
			name: "override wins",
			info: ModelInfo{ID: "some/model", SupportsTools: true, InputCostPer1M: 10},
			overrides: map[string]Profile{
				"some/model": {Tier: TierSmall},
			},
			wantTier: TierSmall,
		},

		// ── toolless gate ─────────────────────────────────────────────────
		{
			name:     "no tools → toolless",
			info:     ModelInfo{ID: "openai/o1-mini", SupportsTools: false, InputCostPer1M: 3},
			wantTier: TierToolless,
		},

		// ── pricing tiers ─────────────────────────────────────────────────
		{
			name:     "frontier by input cost",
			info:     ModelInfo{ID: "anthropic/claude-opus-4", SupportsTools: true, InputCostPer1M: 15},
			wantTier: TierFrontier,
		},
		{
			name:     "frontier by output cost",
			info:     ModelInfo{ID: "x/pricey", SupportsTools: true, OutputCostPer1M: 30},
			wantTier: TierFrontier,
		},
		{
			name:     "large by input cost",
			info:     ModelInfo{ID: "x/model", SupportsTools: true, InputCostPer1M: 2},
			wantTier: TierLarge,
		},
		{
			name:     "large by output cost",
			info:     ModelInfo{ID: "x/model", SupportsTools: true, OutputCostPer1M: 8},
			wantTier: TierLarge,
		},
		{
			name:     "mid by input cost",
			info:     ModelInfo{ID: "x/model", SupportsTools: true, InputCostPer1M: 0.5},
			wantTier: TierMid,
		},
		{
			name:     "mid by output cost",
			info:     ModelInfo{ID: "x/model", SupportsTools: true, OutputCostPer1M: 1.5},
			wantTier: TierMid,
		},
		{
			name:     "small — zero pricing",
			info:     ModelInfo{ID: "meta-llama/llama-3.1-8b-instruct", SupportsTools: true},
			wantTier: TierSmall,
		},
		{
			name:     "small — cheap pricing",
			info:     ModelInfo{ID: "x/tiny", SupportsTools: true, InputCostPer1M: 0.05, OutputCostPer1M: 0.10},
			wantTier: TierSmall,
		},

		// ── free-tier demotion ────────────────────────────────────────────
		{
			name:     "free suffix demotes large→mid",
			info:     ModelInfo{ID: "anthropic/claude-opus-4:free", SupportsTools: true, InputCostPer1M: 15},
			wantTier: TierMid,
		},
		{
			name:     "free suffix demotes frontier→mid",
			info:     ModelInfo{ID: "google/gemini-2.5-pro:free", SupportsTools: true, InputCostPer1M: 7},
			wantTier: TierMid,
		},
		{
			name:     "zero costs with tools stays small (free demotion only caps large+)",
			info:     ModelInfo{ID: "x/model", SupportsTools: true, InputCostPer1M: 0, OutputCostPer1M: 0},
			wantTier: TierSmall,
		},
		{
			name: "non-free mid stays mid",
			info: ModelInfo{ID: "x/mid-model", SupportsTools: true, InputCostPer1M: 0.5},
			wantTier: TierMid,
		},

		// ── context-window bump ───────────────────────────────────────────
		{
			name:     "small pricing + huge ctx → mid",
			info:     ModelInfo{ID: "x/long-ctx", SupportsTools: true, InputCostPer1M: 0.05, ContextWindow: 1_000_000},
			wantTier: TierMid,
		},
		{
			name:     "small pricing + small ctx stays small",
			info:     ModelInfo{ID: "x/tiny", SupportsTools: true, InputCostPer1M: 0.05, ContextWindow: 8_000},
			wantTier: TierSmall,
		},
		{
			name:     "mid pricing + huge ctx stays mid (no over-bump)",
			info:     ModelInfo{ID: "x/mid-long", SupportsTools: true, InputCostPer1M: 0.5, ContextWindow: 1_000_000},
			wantTier: TierMid,
		},

		// ── MaxTokens bumps ───────────────────────────────────────────────
		{
			// GPT-5.5 scenario: no pricing yet, 1.2M ctx, 64k max output → Large
			name:     "no pricing + huge ctx + large max output → large",
			info:     ModelInfo{ID: "openai/gpt-5.5", SupportsTools: true, ContextWindow: 1_200_000, MaxTokens: 65_536},
			wantTier: TierLarge,
		},
		{
			name:     "no pricing + small ctx + large max output → large",
			info:     ModelInfo{ID: "x/capable", SupportsTools: true, MaxTokens: 65_536},
			wantTier: TierLarge,
		},
		{
			name:     "mid pricing + large max output → large",
			info:     ModelInfo{ID: "x/mid", SupportsTools: true, InputCostPer1M: 0.5, MaxTokens: 65_536},
			wantTier: TierLarge,
		},
		{
			// MaxTokens at the 4096 fallback → treat as unknown, no bump
			name:     "4096 fallback max output — no bump",
			info:     ModelInfo{ID: "x/unknown", SupportsTools: true, MaxTokens: 4096},
			wantTier: TierSmall,
		},
		{
			// MaxTokens exactly at threshold — no bump (must be strictly > 32768)
			name:     "32768 max output — no bump",
			info:     ModelInfo{ID: "x/mid", SupportsTools: true, MaxTokens: 32_768},
			wantTier: TierSmall,
		},
		{
			// Frontier pricing → stay Frontier regardless of MaxTokens
			name:     "frontier pricing + low max output stays frontier (clamp in profile, not tier)",
			info:     ModelInfo{ID: "x/frontier", SupportsTools: true, InputCostPer1M: 10.0, MaxTokens: 8192},
			wantTier: TierFrontier,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ClassifyModel(tc.info, tc.overrides)
			if p.Tier != tc.wantTier {
				t.Errorf("ClassifyModel(%q) tier = %q, want %q", tc.info.ID, p.Tier, tc.wantTier)
			}
		})
	}
}

func TestProfileForTierDefaults(t *testing.T) {
	tiers := []Tier{TierSmall, TierMid, TierLarge, TierFrontier}
	prevMaxOut := 0
	prevMaxTurns := 0

	for _, tier := range tiers {
		p := profileForTier(tier)
		if p.Tier != tier {
			t.Errorf("profileForTier(%q).Tier = %q", tier, p.Tier)
		}
		if p.MaxOutputTokens <= prevMaxOut {
			t.Errorf("tier %q MaxOutputTokens (%d) not > previous (%d)", tier, p.MaxOutputTokens, prevMaxOut)
		}
		if p.RecommendedMaxTurns <= prevMaxTurns {
			t.Errorf("tier %q RecommendedMaxTurns (%d) not > previous (%d)", tier, p.RecommendedMaxTurns, prevMaxTurns)
		}
		prevMaxOut = p.MaxOutputTokens
		prevMaxTurns = p.RecommendedMaxTurns
	}
}

func TestSmallProfileTemperature(t *testing.T) {
	p := profileForTier(TierSmall)
	if p.TemperatureDefault == nil {
		t.Fatal("TierSmall TemperatureDefault should be non-nil")
	}
	if *p.TemperatureDefault != 0.3 {
		t.Errorf("TierSmall TemperatureDefault = %v, want 0.3", *p.TemperatureDefault)
	}
}

func TestMidAndAboveNoTemperature(t *testing.T) {
	for _, tier := range []Tier{TierMid, TierLarge, TierFrontier} {
		p := profileForTier(tier)
		if p.TemperatureDefault != nil {
			t.Errorf("tier %q TemperatureDefault should be nil, got %v", tier, *p.TemperatureDefault)
		}
	}
}

func TestClassifyModelMaxOutputClamp(t *testing.T) {
	// Frontier-priced model with small actual output ceiling: profile must clamp.
	m := ModelInfo{ID: "x/limited", SupportsTools: true, InputCostPer1M: 10.0, MaxTokens: 6000}
	p := ClassifyModel(m, nil)
	if p.Tier != TierFrontier {
		t.Fatalf("expected TierFrontier, got %q", p.Tier)
	}
	if p.MaxOutputTokens != 6000 {
		t.Errorf("MaxOutputTokens should be clamped to 6000, got %d", p.MaxOutputTokens)
	}

	// Model with MaxTokens above tier default: no clamp, tier default wins.
	m2 := ModelInfo{ID: "x/big", SupportsTools: true, InputCostPer1M: 10.0, MaxTokens: 65_536}
	p2 := ClassifyModel(m2, nil)
	want := profileForTier(TierFrontier).MaxOutputTokens // 16384
	if p2.MaxOutputTokens != want {
		t.Errorf("MaxOutputTokens should stay at tier default %d, got %d", want, p2.MaxOutputTokens)
	}

	// Unknown MaxTokens (4096 fallback): no clamp, tier default wins.
	m3 := ModelInfo{ID: "x/unknown", SupportsTools: true, InputCostPer1M: 10.0, MaxTokens: 4096}
	p3 := ClassifyModel(m3, nil)
	if p3.MaxOutputTokens != want {
		t.Errorf("fallback MaxTokens 4096 should not clamp tier default %d, got %d", want, p3.MaxOutputTokens)
	}
}

func TestApplyMaxTokens(t *testing.T) {
	const sysDefault = 8096
	mid := profileForTier(TierMid) // MaxOutputTokens = 4096

	// At system default → profile's tier default takes over.
	if got := mid.ApplyMaxTokens(sysDefault, sysDefault); got != 4096 {
		t.Errorf("system default should yield profile default 4096, got %d", got)
	}
	// User explicitly sets a value above the profile default → honored.
	if got := mid.ApplyMaxTokens(8192, sysDefault); got != 8192 {
		t.Errorf("explicit 8192 should be honored, got %d", got)
	}
	// User explicitly sets a value below the profile default → honored.
	if got := mid.ApplyMaxTokens(2048, sysDefault); got != 2048 {
		t.Errorf("explicit 2048 should be honored, got %d", got)
	}
	// Zero current → profile default.
	if got := mid.ApplyMaxTokens(0, sysDefault); got != 4096 {
		t.Errorf("zero current should yield profile default, got %d", got)
	}
	// Zero profile → return current.
	zero := Profile{}
	if got := zero.ApplyMaxTokens(sysDefault, sysDefault); got != sysDefault {
		t.Errorf("zero profile should return current %d, got %d", sysDefault, got)
	}
	// Frontier profile at system default → frontier's larger default.
	frontier := profileForTier(TierFrontier) // MaxOutputTokens = 16384
	if got := frontier.ApplyMaxTokens(sysDefault, sysDefault); got != 16384 {
		t.Errorf("frontier at system default should yield 16384, got %d", got)
	}
}

func TestTemperatureFor(t *testing.T) {
	cfg07 := 0.7
	small := profileForTier(TierSmall) // TemperatureDefault = 0.3

	// User config always wins over tier default.
	if got := small.TemperatureFor(ThinkingLevelOff, &cfg07); got == nil || *got != 0.7 {
		t.Errorf("configTemp should win: got %v", got)
	}
	// Tier default applies when no user config.
	if got := small.TemperatureFor(ThinkingLevelOff, nil); got == nil || *got != 0.3 {
		t.Errorf("tier default should apply: got %v", got)
	}
	// Thinking overrides everything.
	if got := small.TemperatureFor(ThinkingLevelMedium, &cfg07); got != nil {
		t.Errorf("thinking should return nil, got %v", *got)
	}
	// Mid tier has no default; nil config → nil temp.
	mid := profileForTier(TierMid)
	if got := mid.TemperatureFor(ThinkingLevelOff, nil); got != nil {
		t.Errorf("mid with no config should return nil, got %v", *got)
	}
}

func TestMaxTurnsOr(t *testing.T) {
	frontier := profileForTier(TierFrontier) // RecommendedMaxTurns = 60

	// Explicit positive value always wins.
	if got := frontier.MaxTurnsOr(10); got != 10 {
		t.Errorf("explicit 10 should win, got %d", got)
	}
	// 0 defers to profile default.
	if got := frontier.MaxTurnsOr(0); got != 60 {
		t.Errorf("0 should defer to profile (60), got %d", got)
	}
	// -1 sentinel → explicitly unlimited (returns 0).
	if got := frontier.MaxTurnsOr(-1); got != 0 {
		t.Errorf("-1 sentinel should return 0 (unlimited), got %d", got)
	}
	// Zero profile + 0 explicit → 0 (unlimited).
	zero := Profile{}
	if got := zero.MaxTurnsOr(0); got != 0 {
		t.Errorf("zero profile + 0 explicit should be 0 (unlimited), got %d", got)
	}
	// Zero profile + -1 → still 0.
	if got := zero.MaxTurnsOr(-1); got != 0 {
		t.Errorf("zero profile + -1 should be 0 (unlimited), got %d", got)
	}
}

func TestToollessProfile(t *testing.T) {
	p := ClassifyModel(ModelInfo{ID: "openai/o1", SupportsTools: false, InputCostPer1M: 15}, nil)
	if p.Tier != TierToolless {
		t.Errorf("no-tools model should be TierToolless, got %q", p.Tier)
	}
	if p.RecommendedMaxTurns != 1 {
		t.Errorf("TierToolless should have RecommendedMaxTurns=1, got %d", p.RecommendedMaxTurns)
	}
}
