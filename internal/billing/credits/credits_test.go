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
