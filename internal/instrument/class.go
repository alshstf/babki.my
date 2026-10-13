package instrument

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
)

// Sector is a paper's sector in the program's own short list (decision Р-46):
// the broker's and Yahoo's words are brought to it, and a fund goes by what it
// holds — a broad index fund to its own line, not to "other", which it would
// swell.
type Sector string

const (
	SectorFinancials  Sector = "financials"
	SectorEnergy      Sector = "energy"
	SectorIT          Sector = "it"
	SectorConsumer    Sector = "consumer"
	SectorTelecom     Sector = "telecom"
	SectorUtilities   Sector = "utilities"
	SectorMaterials   Sector = "materials"
	SectorIndustrials Sector = "industrials"
	SectorHealthCare  Sector = "health_care"
	SectorRealEstate  Sector = "real_estate"
	SectorGovernment  Sector = "government"
	SectorMunicipal   Sector = "municipal"
	SectorFundsEquity Sector = "funds_equity"
	SectorFundsBonds  Sector = "funds_bonds"
	SectorFundsMoney  Sector = "funds_money"
	SectorFundsGold   Sector = "funds_gold"
	SectorFundsMixed  Sector = "funds_mixed"
	// SectorUnknown: the source gave none, or "other".
	SectorUnknown Sector = ""
)

// ClassSource is where a class came from.
type ClassSource string

const (
	ClassFromTInvest ClassSource = "tinvest"
	ClassFromYahoo   ClassSource = "yahoo"
)

// Class is a paper's country of risk (ISO 3166 code, empty when unknown) and
// sector, with the source's own words in Raw. CountryByHand and SectorByHand
// say the family's own stands over the source's.
type Class struct {
	Country   string
	Sector    Sector
	Source    ClassSource
	Raw       string
	FetchedAt time.Time

	CountryByHand, SectorByHand bool
}

// Sectors is the list, in the order a picker shows it.
var Sectors = []Sector{
	SectorFinancials, SectorEnergy, SectorIT, SectorConsumer, SectorTelecom, SectorUtilities,
	SectorMaterials, SectorIndustrials, SectorHealthCare, SectorRealEstate, SectorGovernment,
	SectorMunicipal, SectorFundsEquity, SectorFundsBonds, SectorFundsMoney, SectorFundsGold, SectorFundsMixed,
}

// ErrBadClass: a country not two Latin letters, or a sector off the list.
var ErrBadClass = fmt.Errorf("%w: the country is two Latin letters, the sector one of the list", family.ErrValidation)

// tinvestSectors brings the broker's sector words to the list; its
// "green" ones are their industries, and "other" is none.
var tinvestSectors = map[string]Sector{
	"financial": SectorFinancials, "energy": SectorEnergy, "it": SectorIT, "consumer": SectorConsumer,
	"telecom": SectorTelecom, "utilities": SectorUtilities, "materials": SectorMaterials,
	"industrials": SectorIndustrials, "health_care": SectorHealthCare, "real_estate": SectorRealEstate,
	"government": SectorGovernment, "municipal": SectorMunicipal,
	"ecomaterials": SectorMaterials, "green_energy": SectorUtilities, "green_buildings": SectorRealEstate,
	"electrocars": SectorConsumer,
}

// fundFocuses brings a fund's focus at the broker to the list. Its gold funds
// are "alternative_investment" (TGLD, 2026-10-13).
var fundFocuses = map[string]Sector{
	"equity": SectorFundsEquity, "private_equity": SectorFundsEquity,
	"fixed_income": SectorFundsBonds, "money_market": SectorFundsMoney,
	"commodity": SectorFundsGold, "alternative_investment": SectorFundsGold,
	"real_estate": SectorRealEstate, "mixed_allocation": SectorFundsMixed, "specialty": SectorFundsMixed,
}

// fundNames read a fund's holdings off its name before its focus: the broker
// has a bond fund down as "equity" (AKMB, «Управляемые облигации», 2026-10-13).
var fundNames = []struct {
	re     *regexp.Regexp
	sector Sector
}{
	{regexp.MustCompile(`(?i)облигац|флоатер|офз|\bbonds?\b`), SectorFundsBonds},
	{regexp.MustCompile(`(?i)золот|серебр|металл|\bgold\b`), SectorFundsGold},
	{regexp.MustCompile(`(?i)денежн|ликвидност|money market`), SectorFundsMoney},
}

// SectorFromTInvest is a paper's sector from its passport at T-Invest: a
// fund's from its name, else its focus; anything else's from the broker's
// sector.
func SectorFromTInvest(t Type, name, sector, focus string) Sector {
	if t == TypeETF {
		for _, f := range fundNames {
			if f.re.MatchString(name) {
				return f.sector
			}
		}
		return fundFocuses[strings.ToLower(focus)]
	}
	return tinvestSectors[strings.ToLower(sector)]
}

// yahooSectors brings Yahoo's sector names to the list.
var yahooSectors = map[string]Sector{
	"Technology": SectorIT, "Financial Services": SectorFinancials, "Energy": SectorEnergy,
	"Consumer Cyclical": SectorConsumer, "Consumer Defensive": SectorConsumer, "Healthcare": SectorHealthCare,
	"Industrials": SectorIndustrials, "Basic Materials": SectorMaterials, "Communication Services": SectorTelecom,
	"Utilities": SectorUtilities, "Real Estate": SectorRealEstate,
}

// SectorFromYahoo is a share's sector from Yahoo's name for it.
func SectorFromYahoo(sector string) Sector {
	return yahooSectors[strings.TrimSpace(sector)]
}

// SetClass records a paper's class from a source, over the one before.
func (s *Store) SetClass(ctx context.Context, id uuid.UUID, c Class) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO instrument_classes (instrument_id, country, sector, source, raw, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (instrument_id) DO UPDATE SET country = EXCLUDED.country, sector = EXCLUDED.sector,
			source = EXCLUDED.source, raw = EXCLUDED.raw, fetched_at = EXCLUDED.fetched_at`,
		id, strings.ToUpper(c.Country), string(c.Sector), string(c.Source), c.Raw, c.FetchedAt)
	if err != nil {
		return fmt.Errorf("instrument: set class: %w", err)
	}
	return nil
}

// Classes are the papers' classes by id; a paper without one is absent.
func (s *Store) Classes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Class, error) {
	rows, err := s.db.Query(ctx, `
		SELECT instrument_id, country, sector, source, raw, fetched_at
		FROM instrument_classes WHERE instrument_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("instrument: classes: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]Class{}
	for rows.Next() {
		var id uuid.UUID
		var c Class
		var sector, source string
		if err := rows.Scan(&id, &c.Country, &sector, &source, &c.Raw, &c.FetchedAt); err != nil {
			return nil, fmt.Errorf("instrument: classes: %w", err)
		}
		c.Sector, c.Source = Sector(sector), ClassSource(source)
		out[id] = c
	}
	return out, rows.Err()
}

// ToClassify are the shares, bonds and funds with an ISIN whose class is
// missing or was fetched before olderThan, the missing first, at most limit.
func (s *Store) ToClassify(ctx context.Context, olderThan time.Time, limit int) ([]Instrument, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+cols+` FROM (
			SELECT i.*, c.fetched_at AS class_at
			FROM instruments i LEFT JOIN instrument_classes c ON c.instrument_id = i.id
			WHERE i.type IN ('share', 'bond', 'etf') AND i.isin <> ''
			  AND (c.instrument_id IS NULL OR c.fetched_at < $1)
		) t
		ORDER BY class_at NULLS FIRST, id
		LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("instrument: to classify: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Instrument, error) { return scan(row) })
	if err != nil {
		return nil, fmt.Errorf("instrument: to classify: %w", err)
	}
	return out, nil
}

// ClassesFor are the papers' classes as the space sees them: its own country
// and sector over the source's; a paper with neither is absent.
func (s *Store) ClassesFor(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Class, error) {
	out, err := s.Classes(ctx, ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT instrument_id, country, sector FROM instrument_class_overrides
		WHERE space_id = $1 AND instrument_id = ANY($2)`, spaceID, ids)
	if err != nil {
		return nil, fmt.Errorf("instrument: class overrides: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var country, sector *string
		if err := rows.Scan(&id, &country, &sector); err != nil {
			return nil, fmt.Errorf("instrument: class overrides: %w", err)
		}
		c := out[id]
		if country != nil {
			c.Country, c.CountryByHand = *country, true
		}
		if sector != nil {
			c.Sector, c.SectorByHand = Sector(*sector), true
		}
		out[id] = c
	}
	return out, rows.Err()
}

// SetClassByHand records the space's own country and sector for a paper;
// a nil one keeps the source's, both nil drop the family's own.
func (s *Store) SetClassByHand(ctx context.Context, spaceID, id uuid.UUID, country, sector *string) error {
	if country != nil {
		c := strings.ToUpper(strings.TrimSpace(*country))
		if !isoCountry.MatchString(c) {
			return ErrBadClass
		}
		country = &c
	}
	if sector != nil && !slices.Contains(Sectors, Sector(*sector)) {
		return ErrBadClass
	}
	if _, err := s.ByID(ctx, id); err != nil {
		return err
	}
	if country == nil && sector == nil {
		_, err := s.db.Exec(ctx, `DELETE FROM instrument_class_overrides WHERE space_id = $1 AND instrument_id = $2`, spaceID, id)
		return err
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO instrument_class_overrides (space_id, instrument_id, country, sector) VALUES ($1, $2, $3, $4)
		ON CONFLICT (space_id, instrument_id) DO UPDATE SET country = EXCLUDED.country, sector = EXCLUDED.sector`,
		spaceID, id, country, sector)
	if err != nil {
		return fmt.Errorf("instrument: set class by hand: %w", err)
	}
	return nil
}

var isoCountry = regexp.MustCompile(`^[A-Z]{2}$`)
