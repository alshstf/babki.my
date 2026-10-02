package table

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata/moex"
	"babki.my/babki/internal/platform/money"
)

// MaxPapers is the most codes one request looks up on the exchange.
const MaxPapers = 200

// ErrExchange is the exchange not answering a lookup.
var ErrExchange = errors.New("table: the exchange did not answer")

type exchange interface {
	FindSecurity(ctx context.Context, code string) (moex.Security, bool, error)
}

type catalogWriter interface {
	Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error)
}

// PapersResult is what looking a table's unknown papers up came to.
type PapersResult struct {
	Added    []AddedPaper
	Known    []string // already in the catalog
	NotFound []string // not on the exchange's priced boards
}

// AddedPaper is a paper filed in the catalog from the exchange.
type AddedPaper struct {
	Code       string
	Instrument instrument.Instrument
}

// AddPapers looks each code — a ticker or an ISIN, as a table names a paper —
// up on the exchange and files the ones it finds in the catalog, so the rows
// naming them can be read. A code the catalog already holds is left alone.
func (s *Service) AddPapers(ctx context.Context, codes []string) (PapersResult, error) {
	if len(codes) > MaxPapers {
		return PapersResult{}, fmt.Errorf("%w: at most %d papers at once", family.ErrValidation, MaxPapers)
	}
	var out PapersResult
	seen := map[string]bool{}
	for _, raw := range codes {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		if known, err := s.inCatalog(ctx, code); err != nil {
			return PapersResult{}, err
		} else if known {
			out.Known = append(out.Known, code)
			continue
		}
		sec, ok, err := s.exchange.FindSecurity(ctx, code)
		if err != nil {
			return PapersResult{}, fmt.Errorf("%w: %v", ErrExchange, err)
		}
		if !ok {
			out.NotFound = append(out.NotFound, code)
			continue
		}
		// Named by its ticker in the table, the paper may already be in the
		// catalog under its ISIN, or the other way round.
		if known, err := s.inCatalog(ctx, sec.ISIN); err != nil {
			return PapersResult{}, err
		} else if known {
			out.Known = append(out.Known, code)
			continue
		}
		inst, err := s.file(ctx, sec)
		if errors.Is(err, instrument.ErrTickerTaken) || errors.Is(err, instrument.ErrISINTaken) {
			out.Known = append(out.Known, code)
			continue
		}
		if err != nil {
			return PapersResult{}, err
		}
		out.Added = append(out.Added, AddedPaper{Code: code, Instrument: inst})
	}
	return out, nil
}

func (s *Service) inCatalog(ctx context.Context, code string) (bool, error) {
	if code == "" {
		return false, nil
	}
	var err error
	if isinPattern.MatchString(code) {
		_, err = s.papers.ByISIN(ctx, code)
	} else {
		_, err = s.papers.ByTickerTradable(ctx, code)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// file creates the catalog row for a paper the exchange described.
func (s *Service) file(ctx context.Context, sec moex.Security) (instrument.Instrument, error) {
	inst := instrument.Instrument{
		Type:     instrument.Type(sec.Kind),
		Name:     sec.Name,
		Ticker:   sec.SecID,
		ISIN:     sec.ISIN,
		Currency: sec.Currency,
	}
	if sec.Kind == "bond" && sec.FaceValue.IsPositive() && sec.FaceCurrency != "" {
		face, err := money.Minor(sec.FaceValue.Shift(2))
		if err != nil {
			return instrument.Instrument{}, fmt.Errorf("face value of %s: %w", sec.SecID, err)
		}
		inst.FaceValueMinor, inst.FaceCurrency = &face, &sec.FaceCurrency
	}
	return s.catalog.Create(ctx, inst)
}
