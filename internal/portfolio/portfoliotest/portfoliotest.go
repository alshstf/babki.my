// Package portfoliotest holds helpers the tests of several packages share
// when they read what the portfolio engine computed.
package portfoliotest

import (
	"testing"

	"babki.my/babki/internal/portfolio"
)

// Realized is p's realized result in its one currency; the test fails when a
// disposal settled in another currency and there is no single figure.
func Realized(t *testing.T, p *portfolio.Position) int64 {
	t.Helper()
	minor, inOneCurrency := p.RealizedPnL()
	if !inOneCurrency {
		t.Fatalf("position %s has no realized result in one currency: a disposal settled in another", p.InstrumentID)
	}
	return minor
}
