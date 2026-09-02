package repository

// Gate B — GC/SC mathematical identity (Milestone 1.2).
//
// THE CLAIM THIS GATE MAKES CHECKABLE
//
// A US sweepstakes operator's legal position rests on the promotional entry
// (Gold Coins) and the sweepstakes entry (Sweeps Coins) being THE SAME GAME.
// Same reels, same probabilities, same paytable, same multipliers. The only
// thing allowed to differ is which pocket of the wallet the money moves between
// and what currency the ledger records.
//
// If SC paid even slightly differently from GC — a trimmed multiplier, one
// heavier losing symbol, a different paytable version resolved for one family —
// the promotional play would no longer be a fair depiction of the sweepstakes
// play, and the sweepstakes would no longer be a sweepstakes. That is not a
// defect that costs money. It is a defect that ends the licence.
//
// WHY IT NEEDS A GATE AT ALL, GIVEN THE CODE IS ALREADY CORRECT
//
// It is correct today: game.Spin takes a Paytable and an RNG, game.WinFor takes
// a bare decimal, and internal/game imports nothing that can name a currency.
// The structural enforcement lives in internal/game/currency_agnostic.go and its
// tests, and it is a compile error to add a currency parameter to the draw.
//
// This gate covers what structure cannot: that the two paths, run end to end,
// actually produce the same numbers. A currency-dependent outcome does not have
// to arrive as a parameter — it could arrive as a different game id resolved per
// family, a rounding rule applied to one branch, or an extra RNG draw on one
// side that shifts the whole subsequent stream. None of those change a
// signature. All of them change the money.
//
// HOW THE REPLAY WORKS
//
// Both paths are driven from ONE seeded entropy stream, replayed from the same
// seed. Deterministic entropy is the only way to compare outcome sequences at
// all — and it is confined to this test: CryptoRNG's production constructor
// still reads crypto/rand, and NewCryptoRNGWithSource is the seam the package
// already provides for exactly this. The rejection sampling, the weighted strip
// walk and the evaluator are all the production ones.
//
// The per-spin sequence below mirrors ProcessSpin and settleSpinTx: draw the
// outcome, derive the win, allocate the debit, apply it, allocate the credit,
// apply it. It stops short of SQL because the database is not what is under
// test — the arithmetic is, and settleSpinTx passes these same values through
// to the statement verbatim.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/Gavrielsh/True/internal/domain"
	"github.com/Gavrielsh/True/internal/game"
)

const (
	// identitySpins is how many spins each path replays for the byte-identity
	// comparison. Large enough to hit every symbol including CROWN three-of-a-kind
	// (p = 6.4e-5, so ~3 expected in 50,000), small enough to stay instant.
	identitySpins = 50_000

	// crossCheckDefaultSpins is the per-currency sample for the statistical
	// cross-check on an ordinary `go test ./...`. CI raises it to 10,000,000 via
	// CURRENCY_SPINS.
	crossCheckDefaultSpins = 250_000

	envCurrencySpins  = "CURRENCY_SPINS"
	envCurrencyReport = "CURRENCY_REPORT_PATH"

	defaultCurrencyReportName = "currency-identity-report.json"

	// crossCheckSigmaMultiplier matches Gate A's band width. Same policy, same
	// justification, applied here to the DIFFERENCE of two independent means.
	crossCheckSigmaMultiplier = 4
)

// ----------------------------------------------------------------------------
// Deterministic entropy — test-only
// ----------------------------------------------------------------------------

// seededEntropy is a reproducible byte source for CryptoRNG.
//
// ChaCha8 rather than a trivial counter because the strip walk consumes whole
// uint64s and a low-entropy stream would land on the same symbol every time,
// making the identity comparison vacuous — two paths agreeing on a constant
// prove nothing.
//
// TEST-ONLY, and it must stay that way: math/rand is deterministic by design,
// which is precisely what makes it correct here and catastrophic in production.
// The production constructor NewCryptoRNG reads crypto/rand and is untouched.
type seededEntropy struct {
	src *rand.ChaCha8
}

func newSeededEntropy(seed uint64) *seededEntropy {
	// The loop counter is uint64 rather than int so no conversion is needed:
	// gosec flags int→uint64 even where the range makes it provably safe, and a
	// suppression comment would be a worse answer than not converting at all.
	// 0x9E3779B97F4A7C15 is the 64-bit golden-ratio constant, used here only to
	// decorrelate the four key words of a single seed.
	var key [32]byte
	for i := uint64(0); i < 4; i++ {
		binary.BigEndian.PutUint64(key[i*8:], seed+i*0x9E3779B97F4A7C15)
	}
	return &seededEntropy{src: rand.NewChaCha8(key)}
}

func (s *seededEntropy) Read(p []byte) (int, error) {
	for i := 0; i < len(p); i += 8 {
		var word [8]byte
		binary.BigEndian.PutUint64(word[:], s.src.Uint64())
		copy(p[i:], word[:])
	}
	return len(p), nil
}

// ----------------------------------------------------------------------------
// One replayed spin
// ----------------------------------------------------------------------------

// agnosticPart is everything the two currencies MUST agree on, byte for byte.
// Serialised through JSON so the comparison is literally on bytes rather than
// on a struct equality that a future field could quietly escape.
type agnosticPart struct {
	Index           int             `json:"index"`
	GameID          string          `json:"game_id"`
	PaytableVersion string          `json:"paytable_version"`
	Reels           []string        `json:"reels"`
	Line            string          `json:"line"`
	WinSymbol       string          `json:"win_symbol"`
	Multiplier      decimal.Decimal `json:"multiplier"`
	BetAmount       string          `json:"bet_amount"`
	WinAmount       string          `json:"win_amount"`
}

// currencyPart is everything the two currencies are ALLOWED to differ on.
// Requirement 5 is exactly the assertion that this struct, and nothing else,
// carries the divergence.
type currencyPart struct {
	Family         string   `json:"family"`
	DebitCurrency  []string `json:"debit_currency"`
	DebitAmounts   []string `json:"debit_amounts"`
	CreditCurrency string   `json:"credit_currency"`
	CreditAmount   string   `json:"credit_amount"`
}

type spinTrace struct {
	Agnostic agnosticPart
	Currency currencyPart
}

// replay runs `spins` spins of the production path for one family, from a
// deterministic stream, and returns the full trace.
func replay(t *testing.T, p game.Paytable, family domain.CurrencyFamily, seed uint64, spins int, opening domain.Wallet) []spinTrace {
	t.Helper()

	rng := game.NewCryptoRNGWithSource(newSeededEntropy(seed))
	wallet := opening
	bet := domain.MoneyFromInt(1)
	traces := make([]spinTrace, 0, spins)

	for i := 0; i < spins; i++ {
		// ---- currency-agnostic: exactly what ProcessSpin does ----
		outcome, err := game.Spin(p, rng)
		if err != nil {
			t.Fatalf("%s spin %d: %v", family, i, err)
		}
		winAmount, err := domain.NewMoney(game.WinFor(bet.Decimal(), outcome, domain.MoneyScale))
		if err != nil {
			t.Fatalf("%s spin %d: derive win: %v", family, i, err)
		}

		// ---- currency-dependent: exactly what settleSpinTx does ----
		alloc, err := wallet.AllocateBet(family, bet)
		if err != nil {
			t.Fatalf("%s spin %d: allocate bet (wallet gc=%s scu=%s scr=%s): %v",
				family, i, wallet.GC, wallet.SCUnplayed, wallet.SCRedeemable, err)
		}
		postBet, err := wallet.ApplyBet(alloc)
		if err != nil {
			t.Fatalf("%s spin %d: apply bet: %v", family, i, err)
		}

		cur := currencyPart{Family: family.String()}
		for _, d := range alloc.Debits {
			cur.DebitCurrency = append(cur.DebitCurrency, string(d.Currency))
			cur.DebitAmounts = append(cur.DebitAmounts, d.Amount.String())
		}

		wallet = postBet
		if winAmount.IsPositive() {
			winAlloc, err := postBet.AllocateWin(family, winAmount)
			if err != nil {
				t.Fatalf("%s spin %d: allocate win: %v", family, i, err)
			}
			cur.CreditCurrency = string(winAlloc.Credit.Currency)
			cur.CreditAmount = winAlloc.Credit.Amount.String()
			wallet = postBet.ApplyWin(winAlloc)
		}

		traces = append(traces, spinTrace{
			Agnostic: agnosticPart{
				Index:           i,
				GameID:          outcome.GameID,
				PaytableVersion: outcome.PaytableVersion,
				Reels:           outcome.Reels,
				Line:            string(outcome.Line),
				WinSymbol:       outcome.WinSymbol,
				Multiplier:      outcome.Multiplier,
				BetAmount:       bet.String(),
				WinAmount:       winAmount.String(),
			},
			Currency: cur,
		})
	}
	return traces
}

// fundedWallet returns a wallet that can sustain `spins` unit bets even if every
// one of them loses.
//
// THE SC SPLIT IS DELIBERATE. Funding SC entirely in SC_UNPLAYED looks like the
// obvious choice and is wrong: with unit stakes the balance never runs out
// inside the sample, so the wagering-order rule is never exercised and the
// SC_UNPLAYED → SC_REDEEMABLE overflow — the very divergence this gate exists to
// pin down — never happens. The first draft did exactly that, and
// TestCurrencyDivergenceIsOnlyWalletAndLedger caught it by requiring the
// permitted divergences to actually occur.
//
// So SC opens with a deliberately FRACTIONAL SC_UNPLAYED balance. The run then
// walks all three states in order:
//
//	spins 1..100   one SC_UNPLAYED debit
//	spin  ~101     a SPLIT debit: 0.5 from SC_UNPLAYED, 0.5 from SC_REDEEMABLE
//	spins 102..n   one SC_REDEEMABLE debit
//
// A whole-number opening balance would skip the split entirely, which is the
// single most interesting row in the table.
func fundedWallet(t *testing.T, family domain.CurrencyFamily, spins int) domain.Wallet {
	t.Helper()
	bankroll := domain.MoneyFromInt(int64(spins) + 1)
	zero := domain.MoneyFromInt(0)

	if family == domain.FamilyGC {
		return domain.Wallet{GC: bankroll, SCUnplayed: zero, SCRedeemable: zero}
	}

	unplayed, err := domain.NewMoney(decimal.RequireFromString("100.5"))
	if err != nil {
		t.Fatalf("seed SC_UNPLAYED: %v", err)
	}
	return domain.Wallet{GC: zero, SCUnplayed: unplayed, SCRedeemable: bankroll}
}

// ----------------------------------------------------------------------------
// Requirement 4 — the identity test
// ----------------------------------------------------------------------------

// TestCurrencyModelIdentity replays one seeded stream through the GC and SC
// paths, for every registered game, and requires the currency-agnostic half of
// every spin to be byte-identical.
func TestCurrencyModelIdentity(t *testing.T) {
	games := game.RegisteredGames()
	if len(games) == 0 {
		t.Fatal("no registered games: this gate would pass vacuously")
	}

	for _, p := range games {
		t.Run(p.GameID+"@"+p.Version, func(t *testing.T) {
			const seed = 0x5EED_1DE0_7117

			// The two paths must resolve the SAME table. Looked up through the
			// production entry point rather than reusing `p`, because a per-family
			// lookup is one of the ways this invariant could actually break.
			gcTable, ok := game.Lookup(p.GameID)
			if !ok {
				t.Fatalf("lookup %s failed", p.GameID)
			}
			scTable, ok := game.Lookup(p.GameID)
			if !ok {
				t.Fatalf("lookup %s failed", p.GameID)
			}

			// Paytable identity: hash, declared RTP, and the reel strip itself.
			if gcTable.Hash() != scTable.Hash() {
				t.Fatalf("paytable hash differs by path: GC %s vs SC %s", gcTable.Hash(), scTable.Hash())
			}
			if !gcTable.DeclaredRTP.Equal(scTable.DeclaredRTP) {
				t.Errorf("DeclaredRTP differs: GC %s vs SC %s", gcTable.DeclaredRTP, scTable.DeclaredRTP)
			}
			if !gcTable.TheoreticalRTP().Equal(scTable.TheoreticalRTP()) {
				t.Errorf("TheoreticalRTP differs: GC %s vs SC %s",
					gcTable.TheoreticalRTP(), scTable.TheoreticalRTP())
			}
			if gcTable.CanonicalForm() != scTable.CanonicalForm() {
				t.Errorf("reel strips differ between paths:\nGC:\n%s\nSC:\n%s",
					gcTable.CanonicalForm(), scTable.CanonicalForm())
			}
			if len(gcTable.Symbols) != len(scTable.Symbols) {
				t.Fatalf("symbol count differs: GC %d vs SC %d", len(gcTable.Symbols), len(scTable.Symbols))
			}
			for i := range gcTable.Symbols {
				a, b := gcTable.Symbols[i], scTable.Symbols[i]
				if a.ID != b.ID || a.Weight != b.Weight ||
					!a.PayThree.Equal(b.PayThree) || !a.PayTwo.Equal(b.PayTwo) {
					t.Errorf("symbol %d differs between paths: GC %+v vs SC %+v", i, a, b)
				}
			}

			gc := replay(t, gcTable, domain.FamilyGC, seed, identitySpins, fundedWallet(t, domain.FamilyGC, identitySpins))
			sc := replay(t, scTable, domain.FamilySC, seed, identitySpins, fundedWallet(t, domain.FamilySC, identitySpins))

			if len(gc) != len(sc) {
				t.Fatalf("trace lengths differ: GC %d vs SC %d", len(gc), len(sc))
			}

			// BYTE-IDENTICAL. Serialising each spin's agnostic half and comparing
			// the bytes means a field added later is covered automatically — a
			// struct comparison would silently ignore it.
			mismatches := 0
			for i := range gc {
				gcBytes, err := json.Marshal(gc[i].Agnostic)
				if err != nil {
					t.Fatalf("marshal GC trace %d: %v", i, err)
				}
				scBytes, err := json.Marshal(sc[i].Agnostic)
				if err != nil {
					t.Fatalf("marshal SC trace %d: %v", i, err)
				}
				if string(gcBytes) != string(scBytes) {
					mismatches++
					if mismatches <= 3 {
						t.Errorf("spin %d diverges between currencies:\n  GC: %s\n  SC: %s",
							i, gcBytes, scBytes)
					}
				}
			}
			if mismatches != 0 {
				t.Fatalf("%d of %d spins differ between the GC and SC paths; "+
					"the promotional game and the sweepstakes game are NOT the same game",
					mismatches, len(gc))
			}

			// A run in which nothing ever paid would compare a long sequence of
			// zeroes and prove very little, so require the sample to have actually
			// exercised the paying branches.
			wins, threes := 0, 0
			for _, tr := range gc {
				if tr.Agnostic.Multiplier.IsPositive() {
					wins++
				}
				if tr.Agnostic.Line == string(game.LineThree) {
					threes++
				}
			}
			if wins == 0 || threes == 0 {
				t.Errorf("sample exercised no paying lines (wins=%d, three-of-a-kind=%d); "+
					"identity over losses alone proves little", wins, threes)
			}
			t.Logf("%s v%s: %d spins byte-identical across GC and SC (%d wins, %d three-of-a-kind), hash %s",
				p.GameID, p.Version, len(gc), wins, threes, gcTable.Hash())
		})
	}
}

// ----------------------------------------------------------------------------
// Requirement 5 — divergence isolation
// ----------------------------------------------------------------------------

// TestCurrencyDivergenceIsOnlyWalletAndLedger asserts the two paths differ in
// exactly the two permitted respects and in no other: which wallet account the
// stake is drawn from, and what currency the ledger entry records.
func TestCurrencyDivergenceIsOnlyWalletAndLedger(t *testing.T) {
	for _, p := range game.RegisteredGames() {
		t.Run(p.GameID+"@"+p.Version, func(t *testing.T) {
			const seed = 0xDEC0_DE01_DE55

			gc := replay(t, p, domain.FamilyGC, seed, identitySpins, fundedWallet(t, domain.FamilyGC, identitySpins))
			sc := replay(t, p, domain.FamilySC, seed, identitySpins, fundedWallet(t, domain.FamilySC, identitySpins))

			sawUnplayed, sawRedeemableDebit, sawCredit := false, false, false

			for i := range gc {
				g, s := gc[i], sc[i]

				// The amounts must match even though the accounts do not. A path
				// that debited a different SUM would be a divergence in the money,
				// not merely in the bookkeeping.
				if g.Agnostic.BetAmount != s.Agnostic.BetAmount {
					t.Fatalf("spin %d: bet amounts differ: GC %s vs SC %s",
						i, g.Agnostic.BetAmount, s.Agnostic.BetAmount)
				}
				if g.Agnostic.WinAmount != s.Agnostic.WinAmount {
					t.Fatalf("spin %d: win amounts differ: GC %s vs SC %s",
						i, g.Agnostic.WinAmount, s.Agnostic.WinAmount)
				}
				if sumStrings(t, g.Currency.DebitAmounts) != sumStrings(t, s.Currency.DebitAmounts) {
					t.Fatalf("spin %d: total debited differs: GC %v vs SC %v",
						i, g.Currency.DebitAmounts, s.Currency.DebitAmounts)
				}

				// PERMITTED DIVERGENCE 1 — the wallet account the stake comes from.
				// GC draws from exactly one account. SC draws SC_UNPLAYED first and
				// overflows into SC_REDEEMABLE, so it may produce two debits.
				if len(g.Currency.DebitCurrency) != 1 || g.Currency.DebitCurrency[0] != string(domain.CurrencyGC) {
					t.Errorf("spin %d: GC debit must be exactly one GC entry, got %v",
						i, g.Currency.DebitCurrency)
				}
				for _, c := range s.Currency.DebitCurrency {
					switch domain.Currency(c) {
					case domain.CurrencySCUnplayed:
						sawUnplayed = true
					case domain.CurrencySCRedeemable:
						sawRedeemableDebit = true
					default:
						t.Errorf("spin %d: SC debit drew on %s; only SC_UNPLAYED and "+
							"SC_REDEEMABLE are permitted", i, c)
					}
				}
				if len(s.Currency.DebitCurrency) == 2 &&
					s.Currency.DebitCurrency[0] != string(domain.CurrencySCUnplayed) {
					t.Errorf("spin %d: a split SC debit must take SC_UNPLAYED first, got %v",
						i, s.Currency.DebitCurrency)
				}

				// PERMITTED DIVERGENCE 2 — the ledger entry currency for the win.
				// SC winnings ALWAYS credit SC_REDEEMABLE: that is what makes won
				// tokens eligible for prize redemption, and it must never be
				// SC_UNPLAYED.
				if g.Currency.CreditCurrency != s.Currency.CreditCurrency {
					sawCredit = true
					if g.Currency.CreditCurrency != string(domain.CurrencyGC) {
						t.Errorf("spin %d: GC win credited %s", i, g.Currency.CreditCurrency)
					}
					if s.Currency.CreditCurrency != string(domain.CurrencySCRedeemable) {
						t.Errorf("spin %d: SC win credited %s; SC wins must always credit "+
							"SC_REDEEMABLE", i, s.Currency.CreditCurrency)
					}
				}
				if s.Currency.CreditCurrency == string(domain.CurrencySCUnplayed) {
					t.Fatalf("spin %d: an SC win credited SC_UNPLAYED — promotional tokens "+
						"cannot be minted by winning", i)
				}
			}

			// The permitted divergences must have actually occurred, or this test
			// asserted nothing about them.
			if !sawUnplayed {
				t.Error("no SC_UNPLAYED debit was observed; the wagering-order rule was never exercised")
			}
			if !sawRedeemableDebit {
				t.Error("no SC_REDEEMABLE debit was observed; the overflow path was never exercised")
			}
			if !sawCredit {
				t.Error("no win was credited; the ledger-currency divergence was never exercised")
			}
			t.Logf("%s v%s: divergence confined to wallet account and ledger currency "+
				"(SC_UNPLAYED debit: %v, SC_REDEEMABLE overflow: %v, win credit: %v)",
				p.GameID, p.Version, sawUnplayed, sawRedeemableDebit, sawCredit)
		})
	}
}

func sumStrings(t *testing.T, xs []string) string {
	t.Helper()
	total := decimal.Zero
	for _, x := range xs {
		d, err := decimal.NewFromString(x)
		if err != nil {
			t.Fatalf("parse amount %q: %v", x, err)
		}
		total = total.Add(d)
	}
	return total.String()
}

// ----------------------------------------------------------------------------
// Requirement 6 — statistical cross-check
// ----------------------------------------------------------------------------

// currencyCrossCheckReport is the machine-readable artifact for this gate.
// Decimals as strings, for the same reason as Gate A's report.
type currencyCrossCheckReport struct {
	SchemaVersion    int    `json:"schema_version"`
	GeneratedAt      string `json:"generated_at"`
	GameID           string `json:"game_id"`
	PaytableVersion  string `json:"paytable_version"`
	PaytableHash     string `json:"paytable_hash"`
	SpinsPerCurrency int64  `json:"spins_per_currency"`
	TheoreticalRTP   string `json:"theoretical_rtp"`
	GCEmpiricalRTP   string `json:"gc_empirical_rtp"`
	SCEmpiricalRTP   string `json:"sc_empirical_rtp"`
	Difference       string `json:"difference"`
	AbsDifference    string `json:"abs_difference"`
	PerSpinStdDev    string `json:"per_spin_stddev"`
	DiffStandardErr  string `json:"difference_standard_error"`
	SigmaMultiplier  int    `json:"sigma_multiplier"`
	Tolerance        string `json:"tolerance"`
	ZScore           string `json:"z_score"`
	WithinTolerance  bool   `json:"within_tolerance"`
}

// TestCurrencyRTPCrossCheck runs an independent sample per currency and requires
// the two empirical returns to agree within analytic noise.
//
// THE BAND. Each path's mean has standard error σ/√n, and the two samples are
// independent, so their DIFFERENCE has variance 2σ²/n:
//
//	SE(diff) = σ · √(2/n)
//	tolerance = 4 · SE(diff)
//
// σ is Paytable.TheoreticalStdDev() — the same closed-form, exhaustively
// verified value Gate A uses. Nothing here is tuned.
//
// Independent streams, not one replayed stream: replaying a single stream is the
// identity test above and would trivially yield a difference of exactly zero,
// which is not a statistical statement. Two independent samples are what makes
// this a cross-check rather than a tautology.
func TestCurrencyRTPCrossCheck(t *testing.T) {
	spins := crossCheckSpins(t)

	for _, p := range game.RegisteredGames() {
		t.Run(p.GameID+"@"+p.Version, func(t *testing.T) {
			sigma, err := p.TheoreticalStdDev()
			if err != nil {
				t.Fatalf("stddev: %v", err)
			}

			start := time.Now()
			gcRTP := sampleRTP(t, p, domain.FamilyGC, 0xA11CE, spins)
			scRTP := sampleRTP(t, p, domain.FamilySC, 0xB0B, spins)
			elapsed := time.Since(start)

			diff := gcRTP.Sub(scRTP)

			// SE(diff) = σ·√(2/n)
			two := decimal.NewFromInt(2)
			ratio := two.Div(decimal.NewFromInt(spins))
			rootRatio, err := ratio.PowWithPrecision(decimal.New(5, -1), 24)
			if err != nil {
				t.Fatalf("sqrt(2/n): %v", err)
			}
			stdErr := sigma.Mul(rootRatio)
			tolerance := decimal.NewFromInt(crossCheckSigmaMultiplier).Mul(stdErr)

			z := decimal.Zero
			if !stdErr.IsZero() {
				z = diff.Div(stdErr)
			}
			within := !diff.Abs().GreaterThan(tolerance)

			writeCurrencyReport(t, currencyReportPath(), currencyCrossCheckReport{
				SchemaVersion:    1,
				GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
				GameID:           p.GameID,
				PaytableVersion:  p.Version,
				PaytableHash:     p.Hash(),
				SpinsPerCurrency: spins,
				TheoreticalRTP:   p.TheoreticalRTP().String(),
				GCEmpiricalRTP:   gcRTP.Round(12).String(),
				SCEmpiricalRTP:   scRTP.Round(12).String(),
				Difference:       diff.Round(12).String(),
				AbsDifference:    diff.Abs().Round(12).String(),
				PerSpinStdDev:    sigma.Round(12).String(),
				DiffStandardErr:  stdErr.Round(12).String(),
				SigmaMultiplier:  crossCheckSigmaMultiplier,
				Tolerance:        tolerance.Round(12).String(),
				ZScore:           z.Round(6).String(),
				WithinTolerance:  within,
			})

			t.Logf("currency cross-check: %d spins per currency in %s", spins, elapsed.Round(time.Millisecond))
			t.Logf("currency cross-check: GC %s | SC %s | theory %s",
				gcRTP.Round(8), scRTP.Round(8), p.TheoreticalRTP())
			t.Logf("currency cross-check: difference %s | SE(diff) %s | tolerance (%dσ) %s | z %s",
				diff.Round(8), stdErr.Round(9), crossCheckSigmaMultiplier,
				tolerance.Round(9), z.Round(3))

			if !within {
				richer := "GC"
				if diff.IsNegative() {
					richer = "SC"
				}
				t.Errorf("CURRENCY RTP DIVERGENCE: %s pays more. GC %s vs SC %s, difference %s "+
					"(%s σ), outside the %dσ band of %s over %d spins per currency",
					richer, gcRTP.Round(8), scRTP.Round(8), diff.Round(8), z.Round(3),
					crossCheckSigmaMultiplier, tolerance.Round(8), spins)
			}
		})
	}
}

// sampleRTP runs `spins` spins of the full per-currency path and returns the
// realised return per unit staked.
//
// Counts outcomes as integers and does the decimal arithmetic once at the end,
// for the same reason as Gate A: a decimal add per spin would dominate the
// runtime and buy nothing.
func sampleRTP(t *testing.T, p game.Paytable, family domain.CurrencyFamily, seed uint64, spins int64) decimal.Decimal {
	t.Helper()

	rng := game.NewCryptoRNGWithSource(newSeededEntropy(seed))
	bet := domain.MoneyFromInt(1)

	symbolIndex := make(map[string]int, len(p.Symbols))
	for i, s := range p.Symbols {
		symbolIndex[s.ID] = i
	}
	three := make([]int64, len(p.Symbols))
	two := make([]int64, len(p.Symbols))

	// The wallet is re-funded every spin rather than carried across the run.
	// Over 10,000,000 spins at an RTP below 1 any finite bankroll drains, and a
	// run that ended in ErrInsufficientFunds would measure the bankroll rather
	// than the game. The allocation path is still exercised on every single spin,
	// which is the point of sampling per currency at all.
	opening := fundedWallet(t, family, 2)

	for i := int64(0); i < spins; i++ {
		outcome, err := game.Spin(p, rng)
		if err != nil {
			t.Fatalf("%s spin %d: %v", family, i, err)
		}
		winAmount, err := domain.NewMoney(game.WinFor(bet.Decimal(), outcome, domain.MoneyScale))
		if err != nil {
			t.Fatalf("%s spin %d: derive win: %v", family, i, err)
		}

		alloc, err := opening.AllocateBet(family, bet)
		if err != nil {
			t.Fatalf("%s spin %d: allocate bet: %v", family, i, err)
		}
		postBet, err := opening.ApplyBet(alloc)
		if err != nil {
			t.Fatalf("%s spin %d: apply bet: %v", family, i, err)
		}
		if winAmount.IsPositive() {
			winAlloc, err := postBet.AllocateWin(family, winAmount)
			if err != nil {
				t.Fatalf("%s spin %d: allocate win: %v", family, i, err)
			}
			_ = postBet.ApplyWin(winAlloc)
		}

		switch outcome.Line {
		case game.LineThree:
			three[symbolIndex[outcome.WinSymbol]]++
		case game.LineTwo:
			two[symbolIndex[outcome.WinSymbol]]++
		case game.LineNone:
		default:
			t.Fatalf("%s spin %d: unknown line %q", family, i, outcome.Line)
		}
	}

	returned := decimal.Zero
	for i, s := range p.Symbols {
		returned = returned.
			Add(decimal.NewFromInt(three[i]).Mul(s.PayThree)).
			Add(decimal.NewFromInt(two[i]).Mul(s.PayTwo))
	}
	return returned.Div(decimal.NewFromInt(spins))
}

func crossCheckSpins(t *testing.T) int64 {
	t.Helper()
	raw, ok := os.LookupEnv(envCurrencySpins)
	if !ok || raw == "" {
		return crossCheckDefaultSpins
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("%s=%q is not an integer: %v", envCurrencySpins, raw, err)
	}
	if n <= 0 {
		t.Fatalf("%s=%d must be > 0", envCurrencySpins, n)
	}
	return n
}

func currencyReportPath() string {
	if p := os.Getenv(envCurrencyReport); p != "" {
		return p
	}
	return defaultCurrencyReportName
}

func writeCurrencyReport(t *testing.T, path string, r currencyCrossCheckReport) {
	t.Helper()
	blob, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal currency report: %v", err)
	}
	blob = append(blob, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create report directory %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write currency report %s: %v", path, err)
	}
	t.Logf("currency cross-check: report written to %s", path)
}

// ----------------------------------------------------------------------------
// The gate's own guards
// ----------------------------------------------------------------------------

// TestSeededEntropyIsDeterministicAndVaried protects the replay harness itself.
//
// If the seeded stream were not reproducible, the identity test would compare
// two different sequences and fail for the wrong reason. If it were constant,
// the test would compare two identical constants and pass for the wrong reason.
// Both are checked, because the second failure mode is silent.
func TestSeededEntropyIsDeterministicAndVaried(t *testing.T) {
	const seed = 42

	a := make([]byte, 4096)
	b := make([]byte, 4096)
	if _, err := newSeededEntropy(seed).Read(a); err != nil {
		t.Fatalf("read a: %v", err)
	}
	if _, err := newSeededEntropy(seed).Read(b); err != nil {
		t.Fatalf("read b: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("the same seed produced different streams; the replay is not reproducible")
	}

	c := make([]byte, 4096)
	if _, err := newSeededEntropy(seed + 1).Read(c); err != nil {
		t.Fatalf("read c: %v", err)
	}
	if string(a) == string(c) {
		t.Fatal("different seeds produced the same stream")
	}

	distinct := map[byte]struct{}{}
	for _, v := range a {
		distinct[v] = struct{}{}
	}
	if len(distinct) < 200 {
		t.Errorf("stream carries only %d distinct byte values; too degenerate to "+
			"make an identity comparison meaningful", len(distinct))
	}

	// And the stream must actually drive varied SPINS, not just varied bytes.
	p := game.ClassicThreeReel
	rng := game.NewCryptoRNGWithSource(newSeededEntropy(seed))
	seen := map[string]struct{}{}
	for i := 0; i < 2000; i++ {
		out, err := game.Spin(p, rng)
		if err != nil {
			t.Fatalf("spin %d: %v", i, err)
		}
		seen[fmt.Sprint(out.Reels)] = struct{}{}
	}
	if len(seen) < 50 {
		t.Errorf("only %d distinct reel combinations in 2000 spins; the seeded "+
			"stream is not exercising the strip", len(seen))
	}
}

// TestCrossCheckBandScalesWithSampleSize proves the cross-check's band is
// derived rather than fixed: the difference of two independent means carries
// √(2/n), so quadrupling the sample must halve the tolerance.
func TestCrossCheckBandScalesWithSampleSize(t *testing.T) {
	p := game.ClassicThreeReel
	sigma, err := p.TheoreticalStdDev()
	if err != nil {
		t.Fatalf("stddev: %v", err)
	}

	band := func(n int64) decimal.Decimal {
		ratio := decimal.NewFromInt(2).Div(decimal.NewFromInt(n))
		root, err := ratio.PowWithPrecision(decimal.New(5, -1), 24)
		if err != nil {
			t.Fatalf("sqrt: %v", err)
		}
		return decimal.NewFromInt(crossCheckSigmaMultiplier).Mul(sigma.Mul(root))
	}

	small, large := band(1_000_000), band(4_000_000)
	ratio := small.Div(large)
	if !ratio.Round(6).Equal(decimal.NewFromInt(2)) {
		t.Errorf("quadrupling n must halve the band: ratio %s (small %s, large %s)",
			ratio.Round(6), small.Round(9), large.Round(9))
	}
	t.Logf("difference band: 1e6 → %s, 4e6 → %s, 1e7 → %s",
		small.Round(9), large.Round(9), band(10_000_000).Round(9))
}
