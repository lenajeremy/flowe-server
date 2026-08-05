package credits

import (
	"testing"

	"workflow-ai/server/internal/database/models"
)

func TestLongestPrefixWinsSoMiniModelsAreNotPricedAsFrontier(t *testing.T) {
	// gpt-4o-mini also matches the gpt-4o prefix. If matching went by table order
	// rather than by specificity, a cheap model would be billed at 4x its rate —
	// and a later edit to the table could reintroduce that silently.
	if mini, full := ModelMultiplier("gpt-4o-mini"), ModelMultiplier("gpt-4o"); mini >= full {
		t.Fatalf("gpt-4o-mini (%v) should be cheaper than gpt-4o (%v)", mini, full)
	}
	if flash, pro := ModelMultiplier("gemini-2.5-flash"), ModelMultiplier("gemini-2.5-pro"); flash >= pro {
		t.Fatalf("gemini flash (%v) should be cheaper than pro (%v)", flash, pro)
	}
}

func TestDatedModelIDsResolveWithoutTheirOwnEntry(t *testing.T) {
	// Providers ship dated releases constantly; the table must not need an entry
	// per date or every release day becomes a mispricing.
	if got, want := ModelMultiplier("claude-haiku-4-5-20251001"), ModelMultiplier("claude-haiku"); got != want {
		t.Fatalf("dated haiku priced at %v, want %v", got, want)
	}
	if ModelMultiplier("CLAUDE-OPUS-4-1") != ModelMultiplier("claude-opus") {
		t.Fatal("model matching should be case-insensitive")
	}
}

func TestUnknownModelsCostMoreThanTheCheapestKnownOne(t *testing.T) {
	// An unrecognised model is far more likely to be a new frontier release than a
	// cheap one, so the default has to err upward. Under-charging silently is worse
	// than over-charging visibly, because only one of the two is ever noticed.
	unknown := ModelMultiplier("some-model-we-have-never-seen")
	if unknown <= ModelMultiplier("claude-haiku") {
		t.Fatalf("unknown model multiplier %v must exceed the cheapest known tier", unknown)
	}
}

func TestOutputTokensCostMoreThanInput(t *testing.T) {
	in := ForTokens("claude-haiku", 10_000, 0, 0, 0)
	out := ForTokens("claude-haiku", 0, 10_000, 0, 0)
	if out <= in {
		t.Fatalf("10k output (%d) should cost more than 10k input (%d)", out, in)
	}
}

func TestCachedInputIsMuchCheaperThanUncached(t *testing.T) {
	// This is where the prompt-caching work shows up as a real saving. If these
	// were priced the same, the whole optimisation would be invisible in the
	// ledger and nobody could tell whether it worked.
	uncached := ForTokens("claude-sonnet", 100_000, 0, 0, 0)
	cached := ForTokens("claude-sonnet", 0, 0, 100_000, 0)
	if cached*5 > uncached {
		t.Fatalf("cached input (%d) is not meaningfully cheaper than uncached (%d)", cached, uncached)
	}
}

func TestCacheWritesCostMoreThanUncachedInput(t *testing.T) {
	// True on some providers, and the reason caching a rarely-reused prompt can be
	// a net loss. Pricing it below uncached would hide that trade entirely.
	write := ForTokens("claude-sonnet", 0, 0, 0, 10_000)
	uncached := ForTokens("claude-sonnet", 10_000, 0, 0, 0)
	if write <= uncached {
		t.Fatalf("cache write (%d) should cost more than uncached input (%d)", write, uncached)
	}
}

func TestAnyRealUsageCostsAtLeastOneCredit(t *testing.T) {
	// A call that used tokens must never round to free, or a tight loop of tiny
	// calls costs nothing and the meter stops meaning anything.
	if got := ForTokens("claude-haiku", 1, 0, 0, 0); got < 1 {
		t.Fatalf("a 1-token call cost %d credits, want at least 1", got)
	}
	if got := ForTokens("claude-haiku", 0, 0, 1, 0); got < 1 {
		t.Fatalf("a 1-cached-token call cost %d credits, want at least 1", got)
	}
	// And a call that genuinely used nothing costs nothing.
	if got := ForTokens("claude-haiku", 0, 0, 0, 0); got != 0 {
		t.Fatalf("a zero-token call cost %d credits, want 0", got)
	}
}

func TestCostScalesWithTheModelMultiplier(t *testing.T) {
	cheap := ForTokens("claude-haiku", 50_000, 10_000, 0, 0)
	dear := ForTokens("claude-opus", 50_000, 10_000, 0, 0)
	ratio := float64(dear) / float64(cheap)
	want := ModelMultiplier("claude-opus") / ModelMultiplier("claude-haiku")
	// Rounding up to a whole credit means this is approximate, not exact.
	if ratio < want*0.99 || ratio > want*1.01 {
		t.Fatalf("cost ratio %.2f does not track the multiplier ratio %.2f", ratio, want)
	}
}

func TestUnknownPlansGetTheCheapestEntitlements(t *testing.T) {
	// A plan string we do not recognise — a Stripe price renamed, a typo in a
	// manual edit — must never grant more than the free tier.
	for _, p := range []models.Plan{"", "enterprise-legacy", "PRO", "gold"} {
		if got := PlanGrant(p); got != GrantFree {
			t.Fatalf("plan %q granted %d credits, want the free grant %d", p, got, GrantFree)
		}
		if got := MaxTokensCeiling(p); got != MaxTokensCeiling(models.PlanFree) {
			t.Fatalf("plan %q got a ceiling of %d, want the free ceiling", p, got)
		}
	}
}

func TestPaidPlansGrantMoreThanFree(t *testing.T) {
	if !(PlanGrant(models.PlanFree) < PlanGrant(models.PlanPro) &&
		PlanGrant(models.PlanPro) < PlanGrant(models.PlanTeam) &&
		PlanGrant(models.PlanTeam) < PlanGrant(models.PlanBusiness)) {
		t.Fatal("grants must increase monotonically with the plan tier")
	}
	if !(MaxTokensCeiling(models.PlanFree) < MaxTokensCeiling(models.PlanPro) &&
		MaxTokensCeiling(models.PlanPro) < MaxTokensCeiling(models.PlanTeam)) {
		t.Fatal("token ceilings must increase with the plan tier")
	}
}

func TestRunHoldIsAffordableOnItsOwnPlansGrant(t *testing.T) {
	// A reservation larger than the plan's whole monthly allowance would make every
	// run on that plan unstartable — the worst possible failure, since it looks
	// like the product is broken rather than like a limit.
	for _, p := range []models.Plan{models.PlanFree, models.PlanPro, models.PlanTeam, models.PlanBusiness} {
		if hold, grant := HoldForRun(p), PlanGrant(p); hold >= grant {
			t.Fatalf("plan %s: run hold %d exceeds its monthly grant %d", p, hold, grant)
		}
	}
}

// TestGrantsKeepCOGSBelowRevenue is the guard on the mistake this file already
// made once: grants of 500k for Pro and 2M for Team worked out at 172% and 202% of
// revenue in provider cost, so every paid plan lost money when the customer used
// what they bought. Nothing in the code says a plan's price, so the check has to
// state it here.
func TestGrantsKeepCOGSBelowRevenue(t *testing.T) {
	// Kept in step with the pricing page's figures by hand; there is no shared
	// constant because prices live in Stripe, not in the binary.
	const (
		proMonthlyUSD  = 29.0
		teamMonthlyUSD = 99.0
		// Business is contract-priced; this is the floor we would ever sign at.
		businessFloorUSD = 500.0
		// Above this the plan is not viable once Stripe's ~4% and support are
		// counted. Well clear of a target of about 30%.
		maxCOGSFraction = 0.45
	)

	cases := []struct {
		plan    models.Plan
		revenue float64
	}{
		{models.PlanPro, proMonthlyUSD},
		{models.PlanTeam, teamMonthlyUSD},
		{models.PlanBusiness, businessFloorUSD},
	}
	for _, tc := range cases {
		costUSD := float64(PlanGrant(tc.plan)) / CreditsPerDollar
		fraction := costUSD / tc.revenue
		if fraction > maxCOGSFraction {
			t.Fatalf("plan %s: a fully-used allowance costs $%.2f against $%.2f revenue "+
				"(%.0f%% COGS, limit %.0f%%) — this plan loses money when the customer "+
				"uses what they paid for", tc.plan, costUSD, tc.revenue,
				fraction*100, maxCOGSFraction*100)
		}
	}

	// The free grant is acquisition cost, so it has no revenue to sit under — but it
	// must stay small enough that farming our managed provider keys is not worth
	// the effort.
	freeCost := float64(GrantFree) / CreditsPerDollar
	if freeCost > 5.0 {
		t.Fatalf("the free grant costs $%.2f of provider spend per signup, which is "+
			"worth farming", freeCost)
	}
}

func TestTheCreditPegMatchesTheRates(t *testing.T) {
	// CreditsPerDollar is derived from InputPer1k, so the two can silently drift
	// apart when rates are edited. A multiplier-1 model at roughly $1 per million
	// input tokens means 1k input tokens costs $0.001.
	const baselineDollarsPer1kInput = 0.001
	creditsFor1k := ForTokens("claude-haiku", 1000, 0, 0, 0)
	impliedPerDollar := float64(creditsFor1k) / baselineDollarsPer1kInput
	if impliedPerDollar != CreditsPerDollar {
		t.Fatalf("the rates imply %.0f credits per dollar but CreditsPerDollar is %d — "+
			"every plan's margin calculation is now wrong", impliedPerDollar, CreditsPerDollar)
	}
}

func TestSmallTierModelsAreNeverPricedAsFrontier(t *testing.T) {
	// Found in production traffic, not by reading the table: gpt-5.4-mini was
	// billing at the gpt-5 rate of 6x, because the table happened to have entries
	// for gpt-4o-mini and gpt-4.1-mini but not for the gpt-5 family. Small models
	// ship faster than anyone updates a table, so the rule has to be general.
	frontierVsSmall := []struct{ small, big string }{
		{"gpt-5.4-mini", "gpt-5.5"},
		{"gpt-5-mini", "gpt-5"},
		{"gpt-5.5-nano", "gpt-5.5"},
		{"gpt-4o-mini", "gpt-4o"},
		{"gpt-4.1-mini", "gpt-4.1"},
		{"gemini-3-flash", "gemini-2.5-pro"},
		{"claude-haiku-4-5-20251001", "claude-opus"},
		// A small model from a family we have never heard of must not land on the
		// conservative default meant for unknown frontier models.
		{"newprovider-7-mini", "gpt-5.5"},
	}
	for _, tc := range frontierVsSmall {
		small, big := ModelMultiplier(tc.small), ModelMultiplier(tc.big)
		if small >= big {
			t.Fatalf("%s costs %vx, not less than %s at %vx", tc.small, small, tc.big, big)
		}
		if small > 1 {
			t.Fatalf("%s should be priced at the small tier, got %vx", tc.small, small)
		}
	}
}

func TestTheSmallTierRuleOnlyEverLowersAMultiplier(t *testing.T) {
	// The ceiling must not become a floor. If a provider ever ships something
	// expensive with "lite" in the name, the table entry still has to win downward
	// — but nothing here may push a multiplier UP to the small tier.
	if got := ModelMultiplier("claude-haiku"); got != 1 {
		t.Fatalf("haiku is already 1x in the table, got %v", got)
	}
	// A frontier model without a marker keeps its table value untouched.
	if got := ModelMultiplier("claude-opus"); got != 15 {
		t.Fatalf("opus multiplier changed to %v — the small-tier rule leaked", got)
	}
}
