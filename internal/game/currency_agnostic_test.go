package game

// Structural enforcement tests for the currency-agnostic outcome layer.
// See currency_agnostic.go for what the invariant is and why it matters.
//
// The compile-time pins in that file stop a currency being added as a
// PARAMETER. These tests close the two ways around that:
//
//   - importing a package that defines currencies and reading one from a
//     variable, a constant, or a struct field instead of a parameter;
//   - naming a currency in an exported signature via a type this package
//     defines itself.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// forbiddenImports are packages this one must never depend on.
//
// internal/domain defines Currency, CurrencyFamily and the wallet; importing it
// would put every currency constant one identifier away from the draw, and the
// compile-time signature pins would not notice. internal/repository composes
// the two and would be a cycle besides.
//
// This is a denylist rather than an allowlist on purpose: the game package
// legitimately grows dependencies (a decimal library, crypto, encoding), and an
// allowlist would be edited to silence this test the first time one is added.
// A denylist is only ever edited by someone deliberately reaching for the thing
// the test exists to forbid.
var forbiddenImports = []string{
	"github.com/Gavrielsh/True/internal/domain",
	"github.com/Gavrielsh/True/internal/repository",
}

// currencyWords are the tokens that betray currency awareness in an identifier.
// Matched case-insensitively against exported function signatures.
var currencyWords = []string{"currency", "family", "wallet", "account", "balance"}

// TestGamePackageIsCurrencyBlind asserts this package cannot see a currency at
// all — not as a parameter, and not through an import.
//
// PRODUCTION FILES ONLY. The _test.go files in this package legitimately do
// things production code must not; excluding them keeps the assertion about the
// shipped package rather than the harness around it.
func TestGamePackageIsCurrencyBlind(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, imp.Path.Value, err)
			}
			for _, forbidden := range forbiddenImports {
				if path == forbidden {
					t.Errorf("%s imports %s: the outcome layer must not be able to see a currency. "+
						"If a spin needs information from that package, pass the specific value it needs "+
						"— never the currency, family, or wallet.", name, forbidden)
				}
			}
		}
	}

	// A package with no production files would pass every assertion above
	// vacuously. That is the failure mode this whole file exists to prevent, so
	// it is checked rather than assumed.
	if checked == 0 {
		t.Fatal("no production .go files were examined; the import check proved nothing")
	}
	t.Logf("checked %d production file(s) for forbidden imports", checked)
}

// TestExportedSignaturesCarryNoCurrency scans every exported function and method
// in the package's production files and fails if a parameter or result type
// mentions currency, family, wallet, account or balance.
//
// This catches what the compile-time pins in currency_agnostic.go cannot: a NEW
// exported function that takes a currency. The pins protect the three functions
// named in them; nothing stops somebody adding a fourth.
func TestExportedSignaturesCarryNoCurrency(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	inspected := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() {
				continue
			}
			inspected++

			var fields []*ast.Field
			if fn.Type.Params != nil {
				fields = append(fields, fn.Type.Params.List...)
			}
			if fn.Type.Results != nil {
				fields = append(fields, fn.Type.Results.List...)
			}

			for _, f := range fields {
				rendered := strings.ToLower(renderType(f.Type))
				for _, word := range currencyWords {
					if strings.Contains(rendered, word) {
						t.Errorf("%s: exported func %s has a signature mentioning %q (%q). "+
							"Outcome generation must not know what a spin is wagered in.",
							name, fn.Name.Name, word, rendered)
					}
				}
			}
		}
	}

	if inspected == 0 {
		t.Fatal("no exported functions were inspected; the signature check proved nothing")
	}
	t.Logf("inspected %d exported function signature(s)", inspected)
}

// renderType flattens a type expression to a string for substring matching.
// Deliberately simple: it needs to see identifiers, not resolve types.
func renderType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return renderType(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + renderType(t.X)
	case *ast.ArrayType:
		return "[]" + renderType(t.Elt)
	case *ast.MapType:
		return "map[" + renderType(t.Key) + "]" + renderType(t.Value)
	case *ast.Ellipsis:
		return "..." + renderType(t.Elt)
	case *ast.FuncType:
		var parts []string
		if t.Params != nil {
			for _, f := range t.Params.List {
				parts = append(parts, renderType(f.Type))
			}
		}
		if t.Results != nil {
			for _, f := range t.Results.List {
				parts = append(parts, renderType(f.Type))
			}
		}
		return "func(" + strings.Join(parts, ",") + ")"
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.ChanType:
		return "chan " + renderType(t.Value)
	default:
		return ""
	}
}

// TestPaytableHashIsStableAndDiscriminating checks the certification digest is
// worth quoting: the same table must hash the same every time, and any change
// that alters what a spin pays must change the hash.
func TestPaytableHashIsStableAndDiscriminating(t *testing.T) {
	base := ClassicThreeReel

	// Determinism is checked against an INDEPENDENTLY constructed copy rather
	// than by calling Hash twice on one value: the latter is what staticcheck
	// SA4000 flags, and it is right to — comparing an expression with itself
	// would still pass if Hash cached its result and never recomputed.
	rebuilt := Paytable{
		GameID:           base.GameID,
		Version:          base.Version,
		DeclaredRTP:      base.DeclaredRTP,
		MaxWinMultiplier: base.MaxWinMultiplier,
		Symbols:          append([]Symbol(nil), base.Symbols...),
	}
	if first, second := base.Hash(), rebuilt.Hash(); first != second {
		t.Fatalf("hash is not deterministic: %s != %s", first, second)
	}

	mutations := []struct {
		name   string
		mutate func(Paytable) Paytable
	}{
		{"a payout changes", func(p Paytable) Paytable {
			syms := append([]Symbol(nil), p.Symbols...)
			syms[0].PayThree = syms[0].PayThree.Add(decOne())
			p.Symbols = syms
			return p
		}},
		{"a weight changes", func(p Paytable) Paytable {
			syms := append([]Symbol(nil), p.Symbols...)
			syms[2].Weight++
			p.Symbols = syms
			return p
		}},
		{"the strip is reordered", func(p Paytable) Paytable {
			syms := append([]Symbol(nil), p.Symbols...)
			syms[0], syms[1] = syms[1], syms[0]
			p.Symbols = syms
			return p
		}},
		{"the version is bumped", func(p Paytable) Paytable {
			p.Version = "1.0.1"
			return p
		}},
		{"the declared RTP changes", func(p Paytable) Paytable {
			p.DeclaredRTP = p.DeclaredRTP.Add(decOne())
			return p
		}},
		{"the max win ceiling changes", func(p Paytable) Paytable {
			p.MaxWinMultiplier = p.MaxWinMultiplier.Add(decOne())
			return p
		}},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if got := m.mutate(base).Hash(); got == base.Hash() {
				t.Errorf("hash unchanged after %s — the digest does not discriminate", m.name)
			}
		})
	}
	t.Logf("%s v%s hash = %s", base.GameID, base.Version, base.Hash())
}

// TestRegisteredGamesIsIsolated proves the registry cannot be mutated through a
// caller's handle. A gate that iterates RegisteredGames() would otherwise be one
// careless caller away from measuring a table nobody ships.
func TestRegisteredGamesIsIsolated(t *testing.T) {
	games := RegisteredGames()
	if len(games) == 0 {
		t.Fatal("no registered games")
	}

	before := games[0].Hash()
	games[0].Symbols[0].Weight += 999
	games[0].Version = "tampered"

	fresh := RegisteredGames()
	if fresh[0].Hash() != before {
		t.Errorf("mutating a returned paytable changed the registry: %s != %s",
			fresh[0].Hash(), before)
	}
}

// decOne is a local helper so the mutation table above reads cleanly.
func decOne() decimal.Decimal { return decimal.NewFromInt(1) }
