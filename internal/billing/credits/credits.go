package credits

import (
	"strings"

	"workflow-ai/server/internal/database/models"
)

// What a credit is worth.
//
// Credits are integers throughout. A float balance invites rounding drift that
// shows up months later as an unexplainable few-cent discrepancy, so the unit is
// small enough that integer arithmetic is always exact.
//
// 1 credit = 1/1000th of the cost of 1k input tokens on the cheapest tier. In
// practice the numbers below matter less than the fact that they are all derived
// from one place: when a provider changes its prices, this table changes and
// nothing else does.
//
// These rates are a STARTING POINT, not a measured one. §5 of
// pricing-and-economics.md is explicit that real rates come from two weeks of
// ledger data. What ships here is a defensible placeholder that errs toward
// over-charging slightly, because the recoverable mistake is refunding credits,
// not discovering we sold tokens below cost.

// Cost per 1,000 tokens, in credits, for a baseline model. Per-model multipliers
// scale these.
const (
	InputPer1k  = 10
	OutputPer1k = 50 // output is the expensive direction on every provider

	// Cached input is a fraction of uncached — this is where §2's prompt-caching
	// work shows up as a real saving.
	CacheReadPer1k = 1
	// Cache writes cost MORE than uncached input on some providers, which is why
	// caching a rarely-reused prompt can be a net loss.
	CacheWritePer1k = 13
)

// Nominal charges for non-LLM operations. Our marginal cost here is close to
// zero, so this is a fair-use brake against abuse rather than cost recovery.
// Deliberately low enough to be effectively free at honest volume: metering the
// thing the product is *for* makes people engineer around the pricing instead of
// using the product.
const (
	IntegrationOp = 1
	EmailSend     = 2
	WebToolCall   = 5 // Brave/Jina are billed per call, so this one is real cost
)

// modelMultipliers scale the base rates by how expensive a model actually is.
// Pricing one "LLM operation" flat would mean absorbing a ~100x cost spread
// between a short prompt on a small model and a 200k-token context on a frontier
// one. Multipliers also nudge people toward cheaper models, which improves COGS
// directly.
//
// Keys are matched as prefixes against the model id, so dated releases
// (claude-haiku-4-5-20251001) resolve without an entry each.
var modelMultipliers = []struct {
	prefix string
	mult   float64
}{
	// Anthropic.
	{"claude-opus", 15},
	{"claude-sonnet", 3},
	{"claude-haiku", 1},
	{"claude-fable", 3},

	// OpenAI.
	{"gpt-5.5", 8},
	{"gpt-5", 6},
	{"gpt-4.1-mini", 1},
	{"gpt-4.1", 4},
	{"gpt-4o-mini", 1},
	{"gpt-4o", 4},
	{"o3", 10},

	// Google.
	{"gemini-2.5-pro", 5},
	{"gemini-2.5-flash", 1},
	{"gemini", 2},

	// xAI.
	{"grok-4", 5},
	{"grok", 3},
}

// defaultMultiplier applies to a model we have no entry for. Set above 1 on
// purpose: an unrecognised model is more likely to be a newly released frontier
// one than a cheap one, and under-charging silently is worse than over-charging
// visibly.
const defaultMultiplier = 4

// smallTierMarkers name the cheap variant within a model family. Every provider
// uses one of these words for it, and none of them ever names a frontier model.
//
// This exists because prefix matching alone gets small models badly wrong. Live
// traffic caught gpt-5.4-mini billing at the gpt-5 frontier rate of 15x the small
// tier, purely because no "mini" entry happened to match — and the entries that do
// exist (gpt-4o-mini, gpt-4.1-mini) only cover models someone remembered to add.
// New small models ship constantly, so an explicit table will always be behind.
var smallTierMarkers = []string{"-mini", "-nano", "-flash", "-lite", "-small", "haiku"}

// smallTierMultiplier is what any small-tier model costs.
//
// Applied as a CEILING, never a floor: it can only lower a multiplier, so it
// cannot accidentally make a model cheaper to us than the table already says. A
// model with "mini" in its name being priced above the small tier is always the
// error; the reverse is not.
const smallTierMultiplier = 1

// ModelMultiplier returns the cost multiplier for a model id.
func ModelMultiplier(model string) float64 {
	m := strings.ToLower(strings.TrimSpace(model))
	// Longest prefix wins, so gpt-4o-mini is not captured by gpt-4o. The table is
	// ordered to make this true, but matching on length rather than on order means
	// a future insertion in the wrong place cannot introduce a mispricing.
	best, bestLen := float64(defaultMultiplier), -1
	for _, e := range modelMultipliers {
		if strings.HasPrefix(m, e.prefix) && len(e.prefix) > bestLen {
			best, bestLen = e.mult, len(e.prefix)
		}
	}
	for _, marker := range smallTierMarkers {
		if strings.Contains(m, marker) && best > smallTierMultiplier {
			return smallTierMultiplier
		}
	}
	return best
}

// ForTokens converts one call's token counts into a credit cost.
//
// Rounds UP to a whole credit: a call that used tokens must never cost zero, or a
// tight loop of tiny calls is free and the meter stops meaning anything.
func ForTokens(model string, inputTokens, outputTokens, cachedTokens, cacheWriteTokens int) int64 {
	mult := ModelMultiplier(model)
	// Scaled by 1000 throughout so the per-1k rates divide exactly, then rounded
	// once at the end. Rounding per-kind instead would compound four times.
	milli := float64(inputTokens)*InputPer1k +
		float64(outputTokens)*OutputPer1k +
		float64(cachedTokens)*CacheReadPer1k +
		float64(cacheWriteTokens)*CacheWritePer1k
	cost := milli * mult / 1000
	if cost <= 0 {
		if inputTokens+outputTokens+cachedTokens+cacheWriteTokens > 0 {
			return 1
		}
		return 0
	}
	if c := int64(cost); float64(c) == cost {
		return c
	}
	return int64(cost) + 1
}

// CreditsPerDollar pegs a credit to real money, which is what makes every number
// below auditable instead of a vibe.
//
// Deliberately in DOLLARS even though plans are priced in euros: provider bills
// arrive in USD, so cost is a dollar figure and revenue is a euro one. Every
// margin calculation has to cross that boundary explicitly rather than pretending
// the two units are interchangeable — see EURUSD.
//
// It falls out of the rates above rather than being chosen separately: a
// haiku-class model (multiplier 1) costs about $1 per million input tokens, so 1k
// input tokens is $0.001 and is priced at InputPer1k = 10 credits. That fixes one
// credit at $0.0001 of provider cost.
//
// Whenever the rates change, this constant has to be rechecked with it — the
// grants below are derived from it, and a stale peg turns every plan's margin into
// a guess.
const CreditsPerDollar = 10_000

// EURUSD converts plan revenue to the currency our costs are in.
//
// A deliberately conservative figure: understating what a euro is worth overstates
// our COGS fraction, so a margin check errs toward flagging a problem that is not
// there rather than missing one that is. Not fetched live — a pricing decision
// should not silently change with the exchange rate.
const EURUSD = 1.05

// Monthly grant per plan, sized so provider cost is a KNOWN fraction of revenue.
//
// The target is ~30% of revenue at full utilisation, which leaves room for
// Stripe's ~4% and still holds a defensible gross margin. Most accounts use far
// less than their allowance, so realised COGS is lower; the point of sizing
// against full use is that the worst case is bounded rather than unknown.
//
// An earlier draft of these numbers was 500k for Pro and 2M for Team, which
// worked out at 172% and 202% of revenue — every paid plan lost money if the
// customer actually used what they bought. TestGrantsKeepCOGSBelowRevenue exists
// so that cannot come back unnoticed.
const (
	// Free is customer acquisition cost, not cost recovery: $2.50 of provider
	// spend per signup. Small enough that farming our managed keys is not worth
	// the effort, which is cheaper than building signup friction.
	GrantFree = 25_000
	// $9.00 of provider cost against €29 revenue.
	GrantPro = 90_000
	// GrantTeamPerSeat is charged PER SEAT, because Team is billed per seat.
	//
	// This is the part that keeps the pricing honest. Seats meter nothing we
	// actually spend — our variable cost is tokens burned by unattended agents,
	// which tracks scheduled agents rather than headcount. A flat allowance on a
	// per-seat price would make a three-person team running forty schedules our
	// most expensive customer AND our cheapest. Scaling the allowance with seats
	// keeps COGS at roughly the same fraction of revenue at every team size.
	//
	// $8.00 of provider cost against €25 of revenue per seat.
	GrantTeamPerSeat = 80_000
	// $150.00, against a contract that starts well above that.
	GrantBusiness = 1_500_000
)

// PlanGrant is the credit allowance for one billing period, for a single seat.
//
// Callers with a seat count should use PlanGrantForSeats instead; this exists for
// the paths that only know the plan.
func PlanGrant(p models.Plan) int64 {
	return PlanGrantForSeats(p, 1)
}

// PlanGrantForSeats is the allowance for one billing period at a given seat count.
//
// Seats are clamped to at least one: a subscription with a missing or zero
// quantity must still grant something, or a webhook that arrives without it would
// silently leave a paying customer at zero.
func PlanGrantForSeats(p models.Plan, seats int) int64 {
	if seats < 1 {
		seats = 1
	}
	switch p {
	case models.PlanPro:
		return GrantPro
	case models.PlanTeam:
		return GrantTeamPerSeat * int64(seats)
	case models.PlanBusiness:
		return GrantBusiness
	default:
		// Anything unrecognised is treated as free. An unknown plan string must
		// never grant more than the cheapest tier, and never scales with seats.
		return GrantFree
	}
}

// MaxTokensCeiling bounds a single LLM call, per plan.
//
// This is what makes a credit hold meaningful: an LLM call's true cost cannot be
// known until it returns, so the reservation is a headroom check against a
// plausible worst case. Without a ceiling there is no worst case to check.
func MaxTokensCeiling(p models.Plan) int {
	switch p {
	case models.PlanPro:
		return 32_000
	case models.PlanTeam:
		return 64_000
	case models.PlanBusiness:
		return 200_000
	default:
		return 8_000
	}
}

// HoldForRun is the headroom reserved when a run starts.
//
// Not an estimate of what the run will cost — an agent turn can loop several
// rounds of tool calls, so the true figure is unknowable up front. It is the floor
// below which starting a run is pointless because it would stop partway through.
func HoldForRun(p models.Plan) int64 {
	ceiling := int64(MaxTokensCeiling(p))
	// One worst-case output-heavy call on a mid-tier model.
	return ceiling * OutputPer1k * 3 / 1000
}
