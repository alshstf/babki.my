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
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/allocation"
	"babki.my/babki/internal/budget"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/portfolio"
	"babki.my/babki/internal/receipt"
	"babki.my/babki/internal/taxyear"
	"babki.my/babki/internal/valuables"
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

type loans interface {
	All(ctx context.Context, spaceID uuid.UUID) ([]loan.Terms, error)
}

type cards interface {
	AllTerms(ctx context.Context, spaceID uuid.UUID) ([]creditcard.Terms, error)
	Installments(ctx context.Context, spaceID, accountID uuid.UUID) (map[uuid.UUID]creditcard.Plan, error)
}

type receipts interface {
	All(ctx context.Context, spaceID uuid.UUID) ([]receipt.Receipt, error)
}

type valuableSets interface {
	Entered(ctx context.Context, spaceID, accountID uuid.UUID) (valuables.Storage, []valuables.BankPrice, []valuables.Item, bool, error)
}

type wageEntries interface {
	Entries(ctx context.Context, spaceID uuid.UUID) (map[uuid.UUID]taxyear.Entry, error)
}

type allocations interface {
	Settings(ctx context.Context, spaceID uuid.UUID) (allocation.Settings, error)
}

type budgets interface {
	Limits(ctx context.Context, spaceID uuid.UUID) ([]budget.Limit, error)
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
	loans       loans
	cards       cards
	budgets     budgets
	receipts    receipts
	allocations allocations
	valuables   valuableSets
	wages       wageEntries
	auth        *family.Auth
	sm          *scs.SessionManager
	now         func() time.Time
}

func NewHandler(sp spaces, acc accounts, j journals, inst instruments, ev events, pr prices, cats categories,
	l loans, cc cards, b budgets, rc receipts, al allocations, auth *family.Auth, sm *scs.SessionManager,
) *Handler {
	return &Handler{
		spaces: sp, accounts: acc, journals: j, instruments: inst, events: ev, prices: pr, categories: cats,
		loans: l, cards: cc, budgets: b, receipts: rc, allocations: al, auth: auth, sm: sm, now: time.Now,
	}
}

// WithValuables writes valuables accounts' bars and coins into the export.
func (h *Handler) WithValuables(v valuableSets) *Handler {
	h.valuables = v
	return h
}

// WithWages writes the wage payments' payslips into the export.
func (h *Handler) WithWages(w wageEntries) *Handler {
	h.wages = w
	return h
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
		BudgetLimits:     []apitypes.BudgetLimit{},
		Receipts:         []apitypes.Receipt{},
		WageEntries:      []apitypes.ExportWageEntry{},
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
	// A loan's and a card's terms ride on their account.
	loanTerms, err := h.loans.All(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	cardTerms, err := h.cards.AllTerms(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	loanOf := map[uuid.UUID]loan.Terms{}
	for _, t := range loanTerms {
		loanOf[t.AccountID] = t
	}
	cardOf := map[uuid.UUID]creditcard.Terms{}
	for _, t := range cardTerms {
		cardOf[t.AccountID] = t
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
		out := exportAccount(a, history[a.ID], ops, stated, usernames)
		if t, ok := loanOf[a.ID]; ok {
			out.Loan = nullable.NewNullableWithValue(loan.TermsAPI(t))
			for _, p := range t.Prepayments {
				out.LoanPrepayments = append(out.LoanPrepayments, loan.PrepaymentAPI(p))
			}
		}
		if t, ok := cardOf[a.ID]; ok {
			out.CreditCard = nullable.NewNullableWithValue(creditcard.TermsAPI(t))
			plans, err := h.cards.Installments(ctx, spaceID, a.ID)
			if err != nil {
				return apitypes.SpaceExport{}, err
			}
			out.CreditCardInstallments = creditcard.InstallmentsExport(plans)
		}
		if a.Type == account.TypeValuables && h.valuables != nil {
			st, bank, items, ok, err := h.valuables.Entered(ctx, spaceID, a.ID)
			if err != nil {
				return apitypes.SpaceExport{}, err
			}
			if ok {
				out.Valuables = nullable.NewNullableWithValue(exportValuables(st, bank, items, usernames))
			}
		}
		doc.Accounts = append(doc.Accounts, out)
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
			Archived: c.Archived, Position: c.Position, TaxMark: nullable.NewNullNullable[string](),
		}
		if c.TaxMark != nil {
			out.TaxMark = nullable.NewNullableWithValue(string(*c.TaxMark))
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
	limits, err := h.budgets.Limits(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	for _, l := range limits {
		doc.BudgetLimits = append(doc.BudgetLimits, budget.LimitAPI(l))
	}
	receipts, err := h.receipts.All(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	for _, r := range receipts {
		doc.Receipts = append(doc.Receipts, receipt.API(r))
	}
	if h.wages != nil {
		entries, err := h.wages.Entries(ctx, spaceID)
		if err != nil {
			return apitypes.SpaceExport{}, err
		}
		for id, e := range entries {
			doc.WageEntries = append(doc.WageEntries, apitypes.ExportWageEntry{OperationId: id, GrossMinor: e.Gross, WithheldMinor: e.Withheld})
		}
		slices.SortFunc(doc.WageEntries, func(a, b apitypes.ExportWageEntry) int {
			return strings.Compare(a.OperationId.String(), b.OperationId.String())
		})
	}
	targets, err := h.allocations.Settings(ctx, spaceID)
	if err != nil {
		return apitypes.SpaceExport{}, err
	}
	doc.Allocation.Mode = apitypes.AllocationMode(targets.Mode)
	doc.Allocation.Kinds, doc.Allocation.Currencies = allocation.TargetsAPI(targets.Kinds), allocation.TargetsAPI(targets.Currencies)
	doc.Allocation.Countries, doc.Allocation.Sectors = allocation.TargetsAPI(targets.Countries), allocation.TargetsAPI(targets.Sectors)
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
		KeptByOperations: a.KeptByOperations, ForeignBroker: a.ForeignBroker, TaxOwnerUsername: nullable.NewNullNullable[string](),
		IntermediaryId: nullable.NewNullNullable[string](),
		CreatedAt:      a.CreatedAt, Balances: []apitypes.ExportBalance{}, Operations: []apitypes.ExportOperation{},
		WithheldStated:         []apitypes.ExportWithheldStated{},
		Loan:                   nullable.NewNullNullable[apitypes.LoanTerms](),
		LoanPrepayments:        []apitypes.LoanPrepayment{},
		CreditCardInstallments: []apitypes.ExportCardInstallment{},
		CreditCard:             nullable.NewNullNullable[apitypes.CreditCardTerms](),
		Valuables:              nullable.NewNullNullable[apitypes.ExportValuables](),
	}
	for _, w := range stated {
		out.WithheldStated = append(out.WithheldStated, apitypes.ExportWithheldStated{
			InstrumentId: w.InstrumentID, PaidOn: w.PaidOn.Format(time.DateOnly), TaxMinor: w.TaxMinor,
		})
	}
	if a.OwnerUserID != nil {
		out.OwnerUsername = nullable.NewNullableWithValue(usernames[*a.OwnerUserID])
	}
	if a.TaxOwnerUserID != nil {
		out.TaxOwnerUsername = nullable.NewNullableWithValue(usernames[*a.TaxOwnerUserID])
	}
	if a.Intermediary != nil {
		out.IntermediaryId = nullable.NewNullableWithValue(*a.Intermediary)
	}
	for _, m := range marks {
		out.Balances = append(out.Balances, apitypes.ExportBalance{AsOf: m.AsOf.Format(time.DateOnly), AmountMinor: m.AmountMinor})
	}
	for _, o := range ops {
		out.Operations = append(out.Operations, exportOperation(o, usernames))
	}
	return out
}

// exportValuables is a storage as written in, its pieces' owners by
// username like the journal's.
func exportValuables(st valuables.Storage, bank []valuables.BankPrice, items []valuables.Item, usernames map[uuid.UUID]string) apitypes.ExportValuables {
	whole := valuables.HoldingAPI(valuables.Holding{Storage: st, BankPrices: bank})
	out := apitypes.ExportValuables{Storage: whole.Storage, BankPrices: whole.BankPrices, Items: []apitypes.ExportValuablesItem{}}
	for _, it := range items {
		e := apitypes.ExportValuablesItem{
			Id: it.ID, Kind: string(it.Kind), Metal: string(it.Metal), Weight: it.Weight.String(), Fineness: it.Fineness.StringFixed(1),
			Maker: it.Maker, ForeignMade: it.ForeignMade, Serial: it.Serial, Certificate: it.Certificate, Sealed: it.Sealed,
			BoughtOn: it.BoughtOn.Format(time.DateOnly), CostMinor: it.CostMinor, Currency: it.Currency,
			OwnerUsername: nullable.NewNullNullable[string](), Note: it.Note,
			SoldOn: nullable.NewNullNullable[string](), SoldMinor: nullable.NewNullNullable[int64](),
		}
		if it.SoldOn != nil && it.SoldMinor != nil {
			e.SoldOn = nullable.NewNullableWithValue(it.SoldOn.Format(time.DateOnly))
			e.SoldMinor = nullable.NewNullableWithValue(*it.SoldMinor)
		}
		if it.OwnerUserID != nil {
			e.OwnerUsername = nullable.NewNullableWithValue(usernames[*it.OwnerUserID])
		}
		out.Items = append(out.Items, e)
	}
	return out
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func exportOperation(o operation.Operation, usernames map[uuid.UUID]string) apitypes.ExportOperation {
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
		MemberUsername: nullable.NewNullNullable[string](),
	}
	if o.MemberID != nil {
		if name, ok := usernames[*o.MemberID]; ok {
			out.MemberUsername = nullable.NewNullableWithValue(name)
		}
	}
	if o.CategoryID != nil {
		out.CategoryId = nullable.NewNullableWithValue(*o.CategoryID)
	}
	out.Parts = make([]apitypes.OperationPart, 0, len(o.Parts))
	for _, p := range o.Parts {
		out.Parts = append(out.Parts, apitypes.OperationPart{CategoryId: p.CategoryID, AmountMinor: p.Amount})
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
