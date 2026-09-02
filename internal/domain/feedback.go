package domain

// Gate C — the losses-disguised-as-wins ban, server side.
//
// WHAT AN LDW IS
//
// A "loss disguised as a win" is a spin that pays something back but less than
// it took, presented to the player with the full apparatus of winning: the
// chime, the flashing frame, the coins counting up. The player stakes 1.00,
// two cherries land, 1.00 comes back, and the machine congratulates them on a
// round in which they gained nothing. Do it a few hundred times an hour and the
// player's memory of the session is of winning, while the ledger says otherwise.
//
// It is among the most studied harms in gambling design and is explicitly
// restricted in several jurisdictions. This platform's stance is that a
// celebration requires an actual gain, and that the stance has to be enforced in
// the engine rather than asked for in a style guide.
//
// THIS IS NOT HYPOTHETICAL FOR classic-3reel
//
// Two of its outcomes pay exactly ×1: a leading CHERRY pair and a leading LEMON
// pair. Both return the stake exactly. Their combined probability is 10.99%, so
// on this paytable MORE THAN ONE SPIN IN TEN is a "win" the player is no better
// off for. Those two outcomes are the entire reason this gate exists, and any
// UI that reads `win_amount > 0` to decide whether to celebrate is celebrating
// them today.
//
// WHY THE SERVER DECIDES
//
// The client is given a CLASS, not the arithmetic. If the browser were handed
// `bet` and `win` and left to work out whether to celebrate, then the rule would
// live in whichever component happened to render the result, in a codebase where
// a new one can be added by anybody. Worse, `win_amount > 0` is the obvious
// thing to write and is exactly wrong. Shipping the decision from the engine
// makes the rule a property of the platform rather than a convention the
// frontend is trusted to remember.

// FeedbackClass is the server's verdict on how a settled round may be presented.
// It is the ONLY input a client is permitted to use when deciding whether to
// celebrate.
type FeedbackClass string

const (
	// FeedbackWin — the player finished the round ahead. Strictly requires
	// netPosition > 0. This is the only value that may trigger celebratory
	// sound, animation, or positive framing.
	FeedbackWin FeedbackClass = "WIN"

	// FeedbackNeutral — the round returned exactly what it took. THIS IS THE
	// LDW CASE. A payout occurred, and it is honest to show it, but the player
	// gained nothing and must not be congratulated.
	FeedbackNeutral FeedbackClass = "NEUTRAL"

	// FeedbackLoss — the round returned less than it took. Covers both a
	// zero-return spin and a partial return, which some paytables produce even
	// though this one does not.
	FeedbackLoss FeedbackClass = "LOSS"
)

// Valid reports whether f is one of the three permitted classes. Used at
// serialisation boundaries so an unset or hand-edited value cannot travel.
func (f FeedbackClass) Valid() bool {
	switch f {
	case FeedbackWin, FeedbackNeutral, FeedbackLoss:
		return true
	}
	return false
}

func (f FeedbackClass) String() string { return string(f) }

// NetPosition is what the player actually gained or lost on the round:
// totalReturn − totalStake.
//
// Negative for a losing round, exactly zero for an LDW, positive for a real win.
// It is carried on the wire alongside the class so the classification is
// auditable rather than something the client has to take on faith — a reviewer
// or a regulator can recompute it from the same two numbers.
func NetPosition(totalReturn, totalStake Money) Money {
	return totalReturn.Sub(totalStake)
}

// ClassifyFeedback maps a net position to the presentation class it permits.
//
// Total on the sign of net: every Money value falls into exactly one branch, so
// there is no input for which the class is undefined and no default that could
// quietly become WIN. The ordering is deliberate — the positive case is checked
// FIRST and explicitly, so that WIN is only ever reachable through a strict
// `> 0`, and every other path falls through to something safe.
func ClassifyFeedback(net Money) FeedbackClass {
	switch {
	case net.IsPositive():
		return FeedbackWin
	case net.IsZero():
		return FeedbackNeutral
	default:
		return FeedbackLoss
	}
}

// ValidateFeedback reports whether a (netPosition, class) pair is self-consistent.
//
// FAIL-CLOSED. ClassifyFeedback is the only sanctioned way to produce a class,
// but a struct field can be assigned by anything that can name it, and the field
// travels to the client. This is the check that runs on the way out: if the two
// ever disagree, the response is refused rather than sent. A round nobody can
// classify is a round nobody should be celebrating.
func ValidateFeedback(net Money, class FeedbackClass) error {
	if !class.Valid() {
		return &FeedbackMismatchError{Net: net, Class: class, Want: ClassifyFeedback(net)}
	}
	if want := ClassifyFeedback(net); want != class {
		return &FeedbackMismatchError{Net: net, Class: class, Want: want}
	}
	return nil
}

// FeedbackMismatchError reports a class that does not follow from its net
// position. Carries both values so the log line is enough to diagnose without
// reproducing the spin.
type FeedbackMismatchError struct {
	Net   Money
	Class FeedbackClass
	Want  FeedbackClass
}

func (e *FeedbackMismatchError) Error() string {
	return "domain: feedback_class " + string(e.Class) +
		" does not follow from net_position " + e.Net.String() +
		" (want " + string(e.Want) + ")"
}
