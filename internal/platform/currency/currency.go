// Package currency states, once, what this program accepts as the name of a
// currency at a write.
//
// It is a package of its own for the reason money.MaxAmountMinor is: the rule
// was written out four times, in four Go packages — an account's currency
// (internal/account), an instrument's and its face value's
// (internal/instrument), an operation's (internal/operation) and the space's
// base currency (internal/family) — and four statements of one rule are four
// things that can drift apart. Those four now call Valid below instead of
// restating it, but that was never the whole count. api/openapi.yaml declares
// the same shape as a `pattern` on every currency field a client can validate a
// request against, and TestTheContractStatesTheCurrencyShapeTheServerEnforces
// (contract_sites_test.go) compares the document against Pattern below rather
// than against a copy of it. And three more copies live in the frontend, each
// deciding whether a Save button is even enabled before any request is sent —
// web/src/routes/settings/index.tsx, web/src/routes/accounts/account-dialog.tsx
// and web/src/routes/accounts/instrument-picker.tsx — tied to Pattern the same
// way money.MaxAmountMinor's own frontend copy is tied
// (internal/platform/money/bound_sites_test.go) by
// TestTheCurrencyFormsRefuseAtTheShapeTheServerEnforces (frontend_sites_test.go).
// Eight statements of one rule, not five, and every one of them either imports
// this package or is a test away from silently drifting from it.
//
// WHAT IT CHECKS IS THE SHAPE, NOT THE REGISTER. Three uppercase ASCII letters
// is what an ISO-4217 alphabetic code looks like; whether the three letters name
// an assigned currency is a different question, and this program does not hold
// the list to answer it. "XYZ" is accepted here and always has been. That is a
// deliberate line rather than an oversight: the register changes without this
// program being rebuilt, a currency it did not know would be refused as a typo,
// and every figure this application publishes carries the code it was recorded
// under rather than looking anything up by it. What the shape rules out is the
// class of value the readers cannot survive — an empty string, a lowercase
// spelling that will not match its own rate row, a currency's NAME where its
// code belongs — and the refusals accordingly say "ISO-4217 uppercase", which
// is a statement about the spelling and not a claim that the register was
// consulted.
package currency

import "regexp"

// Pattern is that shape as a string, so the one place it is written can be read
// by something other than Go — the OpenAPI document declares it verbatim.
//
// Anchored at both ends: without the anchors a code would be found INSIDE a
// longer string, and "RUBLE" would pass as "RUB".
const Pattern = `^[A-Z]{3}$`

var codeRe = regexp.MustCompile(Pattern)

// Valid reports whether code has the shape of a currency code (see the package
// doc for what that does and does not promise). Every door in this program that
// writes a currency goes through it, so no two of them can come to accept
// different things.
func Valid(code string) bool { return codeRe.MatchString(code) }
