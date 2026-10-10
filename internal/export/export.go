// Package export writes everything a family has entered as one JSON document:
// the space, its members, its accounts with their balance marks and journals,
// the papers the journals name, the registry's events about them and the
// prices stated by hand. It reads through each module's own store and owns no
// table of its own.
package export

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/portfolio"
)

// Version is the document's format version (see SpaceExport.version).
const Version = 1

type spaces interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
	ListMembers(ctx context.Context, spaceID uuid.UUID) ([]family.Member, error)
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
	BalanceHistory(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID][]account.BalancePoint, error)
}

type journals interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]operation.Operation, error)
	StatedWithheld(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID) ([]operation.StatedWithheld, error)
}

type instruments interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

type events interface {
	List(ctx context.Context) ([]corporateaction.Event, error)
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
	Rules(ctx context.Context, spaceID uuid.UUID) ([]category.Rule, error)
}

type prices interface {
	PriceSeries(ctx context.Context, instrumentID uuid.UUID, from, to time.Time) ([]marketdata.Quote, error)
}

// Handler serves GET /api/v1/export.
type Handler struct {
	spaces      spaces
	accounts    accounts
	journals    journals
	instruments instruments
	events      events
	prices      prices
	categories  categories
	auth        *family.Auth
	sm          *scs.SessionManager
	now         func() time.Time
}

func NewHandler(sp spaces, acc accounts, j journals, inst instruments, ev events, pr prices, cats categories,
	auth *family.Auth, sm *scs.SessionManager,
) *Handler {
	return &Handler{
		spaces: sp, accounts: acc, journals: j, instruments: inst, events: ev, prices: pr, categories: cats,
		auth: auth, sm: sm, now: time.Now,
	}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/export", h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleOwner, http.HandlerFunc(h.handleExport)))))
}

func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	doc, err := h.Space(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="babki-export-%s.json"`, doc.ExportedAt.Format(time.DateOnly)))
	httpjson.Write(w, http.StatusOK, doc)
}

// Space is the whole of one space as the export document.
func (h *Handler) Space(ctx context.Context, spaceID uuid.UUID) (apitypes.SpaceExport, error) {
	sp, err := h.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	members, err := h.spaces.ListMembers(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	doc := apitypes.SpaceExport{
		Format: apitypes.BabkiMyspaceExport, Version: Version, ExportedAt: h.now().UTC(),
		Space: apitypes.ExportSpace{
			Id: sp.ID, Name: sp.Name, BaseCurrency: sp.BaseCurrency, TaxResidency: sp.TaxResidency,
		},
		Members:          []apitypes.ExportMember{},
		Accounts:         []apitypes.ExportAccount{},
		Instruments:      []apitypes.ExportInstrument{},
		InstrumentEvents: []apitypes.ExportInstrumentEvent{},
		ManualPrices:     []apitypes.ExportManualPrice{},
		Categories:       []apitypes.ExportCategory{},
		CategoryRules:    []apitypes.CategoryRule{},
	}
	usernames := make(map[uuid.UUID]string, len(members))
	for _, m := range members {
		usernames[m.ID] = m.Username
		doc.Members = append(doc.Members, apitypes.ExportMember{
			Username: m.Username, DisplayName: m.DisplayName, Role: string(m.Role),
		})
	}

	list, err := h.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	history, err := h.accounts.BalanceHistory(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	papers := map[uuid.UUID]bool{}
	for _, a := range list {
		ops, err := h.journals.ListForEngine(ctx, spaceID, a.ID)
		if err != nil {
			return apitypes.SpaceExport{}, err
		}
		stated, err := h.journals.StatedWithheld(ctx, spaceID, []uuid.UUID{a.ID})
		if err != nil {
			return apitypes.SpaceExport{}, err
		}
		doc.Accounts = append(doc.Accounts, exportAccount(a, history[a.ID], ops, stated, usernames))
		for _, o := range ops {
			if o.InstrumentID != nil {
				papers[*o.InstrumentID] = true
			}
		}
	}

	if err := h.addPapers(ctx, &doc, papers); err != nil {
		return apitypes.SpaceExport{}, err
	}
	// The top level first, then the ones under it, so a reader rebuilding the
	// tree meets every parent before its children; within each, the list's order.
	cats, err := h.categories.List(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	slices.SortStableFunc(cats, func(a, b category.Category) int {
		return cmp.Compare(boolRank(a.ParentID != nil), boolRank(b.ParentID != nil))
	})
	for _, c := range cats {
		out := apitypes.ExportCategory{
			Id: c.ID, Kind: string(c.Kind), Name: c.Name, ParentId: nullable.NewNullNullable[uuid.UUID](),
			Archived: c.Archived, Position: c.Position,
		}
		if c.ParentID != nil {
			out.ParentId = nullable.NewNullableWithValue(*c.ParentID)
		}
		doc.Categories = append(doc.Categories, out)
	}
	rules, err := h.categories.Rules(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	for _, r := range rules {
		doc.CategoryRules = append(doc.CategoryRules, apitypes.CategoryRule{
			Id: r.ID, CategoryId: r.CategoryID, Field: apitypes.CategoryRuleField(r.Field),
			Pattern: r.Pattern, Position: r.Position,
		})
	}
	return doc, nil
}

// addPapers adds the papers the journals name, the registry's events about
// them and the prices stated for them by hand, each in a stable order.
func (h *Handler) addPapers(ctx context.Context, doc *apitypes.SpaceExport, papers map[uuid.UUID]bool) error {
	ids := make([]uuid.UUID, 0, len(papers))
	for id := range papers {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	catalog, err := h.instruments.ByIDs(ctx, ids)
	if err != nil {
		return err
	}
	isins := map[string]bool{}
	today := dates.LatestRecordable()
	for _, id := range ids {
		paper, ok := catalog[id]
		if !ok {
			continue
		}
		doc.Instruments = append(doc.Instruments, exportInstrument(paper))
		if paper.ISIN != "" {
			isins[paper.ISIN] = true
		}
		quotes, err := h.prices.PriceSeries(ctx, id, dates.EarliestRecordable(), today)
		if err != nil {
			return err
		}
		for _, q := range quotes {
			if q.Source == portfolio.ManualPriceSource {
				doc.ManualPrices = append(doc.ManualPrices, apitypes.ExportManualPrice{
					InstrumentId: id, On: q.On.Format(time.DateOnly), Price: q.Price.String(), Currency: q.Currency,
				})
			}
		}
	}
	registry, err := h.events.List(ctx)
	if err != nil {
		return err
	}
	for _, e := range registry {
		if isins[e.ISIN] || (e.ResultISIN != "" && isins[e.ResultISIN]) {
			doc.InstrumentEvents = append(doc.InstrumentEvents, exportEvent(e))
		}
	}
	return nil
}

func exportAccount(a account.WithBalance, marks []account.BalancePoint, ops []operation.Operation,
	stated []operation.StatedWithheld, usernames map[uuid.UUID]string,
) apitypes.ExportAccount {
	out := apitypes.ExportAccount{
		Id: a.ID, Name: a.Name, Type: string(a.Type), Currency: a.Currency, Institution: a.Institution,
		Status: string(a.Status), OwnerUsername: nullable.NewNullNullable[string](), ValuedByBalance: a.ValuedByBalance, TradesAbroad: a.TradesAbroad,
		CreatedAt: a.CreatedAt, Balances: []apitypes.ExportBalance{}, Operations: []apitypes.ExportOperation{},
		WithheldStated: []apitypes.ExportWithheldStated{},
	}
	for _, w := range stated {
		out.WithheldStated = append(out.WithheldStated, apitypes.ExportWithheldStated{
			InstrumentId: w.InstrumentID, PaidOn: w.PaidOn.Format(time.DateOnly), TaxMinor: w.TaxMinor,
		})
	}
	if a.OwnerUserID != nil {
		out.OwnerUsername = nullable.NewNullableWithValue(usernames[*a.OwnerUserID])
	}
	for _, m := range marks {
		out.Balances = append(out.Balances, apitypes.ExportBalance{AsOf: m.AsOf.Format(time.DateOnly), AmountMinor: m.AmountMinor})
	}
	for _, o := range ops {
		out.Operations = append(out.Operations, exportOperation(o))
	}
	return out
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func exportOperation(o operation.Operation) apitypes.ExportOperation {
	out := apitypes.ExportOperation{
		Id: o.ID, Type: string(o.Type), OccurredOn: o.OccurredOn.Format(time.DateOnly),
		SettledOn: nullable.NewNullNullable[string](), InstrumentId: nullable.NewNullNullable[uuid.UUID](),
		Quantity: nullable.NewNullNullable[string](), Price: nullable.NewNullNullable[string](),
		AmountMinor: o.AmountMinor, Currency: o.Currency, FeeMinor: o.FeeMinor, Note: o.Note,
		TradingMode: nullable.NewNullNullable[string](), TransferGroupId: nullable.NewNullNullable[uuid.UUID](),
		SplitRatio: nullable.NewNullNullable[string](), FaceBeforeMinor: nullable.NewNullNullable[int64](),
		Source: o.Source, ExternalId: nullable.NewNullNullable[string](),
		CreatedAt: o.CreatedAt, Lots: []apitypes.ExportLot{},
		CategoryId: nullable.NewNullNullable[uuid.UUID](), Counterparty: o.Counterparty,
	}
	if o.CategoryID != nil {
		out.CategoryId = nullable.NewNullableWithValue(*o.CategoryID)
	}
	if o.SettledOn != nil {
		out.SettledOn = nullable.NewNullableWithValue(o.SettledOn.Format(time.DateOnly))
	}
	if o.InstrumentID != nil {
		out.InstrumentId = nullable.NewNullableWithValue(*o.InstrumentID)
	}
	if o.Quantity != nil {
		out.Quantity = nullable.NewNullableWithValue(o.Quantity.String())
	}
	if o.Price != nil {
		out.Price = nullable.NewNullableWithValue(o.Price.String())
	}
	if o.TradingMode != nil {
		out.TradingMode = nullable.NewNullableWithValue(*o.TradingMode)
	}
	if o.TransferGroupID != nil {
		out.TransferGroupId = nullable.NewNullableWithValue(*o.TransferGroupID)
	}
	if o.SplitRatio != nil {
		out.SplitRatio = nullable.NewNullableWithValue(o.SplitRatio.String())
	}
	if o.FaceBeforeMinor != nil {
		out.FaceBeforeMinor = nullable.NewNullableWithValue(*o.FaceBeforeMinor)
	}
	if o.ExternalID != nil {
		out.ExternalId = nullable.NewNullableWithValue(*o.ExternalID)
	}
	for _, l := range o.TransferLots {
		lot := apitypes.ExportLot{Quantity: l.Quantity.String(), CostMinor: l.CostMinor, AcquiredOn: nullable.NewNullNullable[string]()}
		if l.AcquiredOn != nil {
			lot.AcquiredOn = nullable.NewNullableWithValue(l.AcquiredOn.Format(time.DateOnly))
		}
		out.Lots = append(out.Lots, lot)
	}
	return out
}

func exportInstrument(i instrument.Instrument) apitypes.ExportInstrument {
	out := apitypes.ExportInstrument{
		Id: i.ID, Type: string(i.Type), Name: i.Name, Ticker: i.Ticker, Isin: i.ISIN, Figi: i.FIGI,
		Currency: i.Currency, FaceValueMinor: nullable.NewNullNullable[int64](),
		FaceCurrency: nullable.NewNullNullable[string](), Frozen: i.Frozen,
	}
	if i.FaceValueMinor != nil {
		out.FaceValueMinor = nullable.NewNullableWithValue(*i.FaceValueMinor)
	}
	if i.FaceCurrency != nil {
		out.FaceCurrency = nullable.NewNullableWithValue(*i.FaceCurrency)
	}
	return out
}

func exportEvent(e corporateaction.Event) apitypes.ExportInstrumentEvent {
	out := apitypes.ExportInstrumentEvent{
		Kind: string(e.Kind), Isin: e.ISIN, EffectiveOn: e.EffectiveOn.Format(time.DateOnly),
		RatioFrom: e.RatioFrom, RatioTo: e.RatioTo, ResultIsin: e.ResultISIN,
		BasisShare: nullable.NewNullNullable[string](), Source: e.Source, SourceRef: e.SourceRef, Note: e.Note,
	}
	if e.BasisShare != nil {
		out.BasisShare = nullable.NewNullableWithValue(e.BasisShare.String())
	}
	return out
}
