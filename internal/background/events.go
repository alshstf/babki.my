package background

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/cashback"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/notify"
	"babki.my/babki/internal/payouts"
)

// figureNames are the figure sources as people call them, for a push that
// one stopped (decision Р-61 Б); the screen's names are the web's
// (components/data-sources.tsx).
var figureNames = map[string]string{
	marketdata.RefreshQuotesArgs{}.Kind():       "Цены бумаг Московской биржи",
	marketdata.RefreshFxArgs{}.Kind():           "Курсы валют ЦБ",
	tinvest.RefreshQuotesArgs{}.Kind():          "Цены бумаг от Т-Инвестиций",
	marketdata.RefreshCryptoPricesArgs{}.Kind(): "Цены криптовалют",
}

// payoutWords are the payouts' kinds as a push names them.
var payoutWords = map[payouts.Kind]string{
	payouts.KindCoupon: "купон", payouts.KindAmortization: "амортизация", payouts.KindRedemption: "погашение",
	payouts.KindDividend: "дивиденды",
}

// events tells the reminders what happened to a space's data: its broker
// connections, its sources of prices and rates, its payouts.
type events struct {
	pool        *pgxpool.Pool
	tinvest     *tinvest.Store
	accounts    *account.Store
	instruments *instrument.Store
	payouts     *payouts.Service
	cashback    *cashback.Service
}

var _ notify.EventSource = (*events)(nil)

func (e *events) Reminders(ctx context.Context, spaceID uuid.UUID, today, now time.Time) ([]notify.Reminder, error) {
	accounts, err := e.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	byID := map[uuid.UUID]account.Account{}
	for _, a := range accounts {
		byID[a.ID] = a.Account
	}
	sync, mismatches, err := e.brokers(ctx, spaceID, byID)
	if err != nil {
		return nil, err
	}
	out := append(notify.SyncReminders(sync, today), notify.ReconcileReminders(mismatches)...)
	stale, err := e.stale(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, notify.StaleReminders(stale)...)
	more, err := e.payoutReminders(ctx, spaceID, today, byID)
	if err != nil {
		return nil, err
	}
	out = append(out, more...)
	// A card's categories of cashback for the new period (Р-58).
	if e.cashback != nil {
		due, err := e.cashback.Due(ctx, spaceID, today)
		if err != nil {
			return nil, err
		}
		list := make([]notify.PickDue, 0, len(due))
		for _, d := range due {
			list = append(list, notify.PickDue{AccountID: d.AccountID, Name: d.Name, Owner: d.Owner, From: d.From, To: d.To})
		}
		out = append(out, notify.LoyaltyReminders(list)...)
	}
	return out, nil
}

// brokers are the space's T-Invest connections as their syncs left them,
// and the accounts their last reconciliation found apart.
func (e *events) brokers(ctx context.Context, spaceID uuid.UUID, byID map[uuid.UUID]account.Account) ([]notify.SyncState, []notify.Mismatch, error) {
	if e.tinvest == nil {
		return nil, nil, nil
	}
	conns, err := e.tinvest.ListConnections(ctx, spaceID)
	if err != nil {
		return nil, nil, err
	}
	var sync []notify.SyncState
	var mismatches []notify.Mismatch
	for _, c := range conns {
		if c.Status == tinvest.StatusDisabled {
			continue
		}
		s := notify.SyncState{ID: c.ID, Broker: "Т-Инвестиции", Last4: c.TokenLast4, Revoked: c.Status == tinvest.StatusTokenRevoked, Since: c.UpdatedAt}
		if s.LastOK, err = e.tinvest.LastSuccessfulSyncAt(ctx, c.ID); err != nil {
			return nil, nil, err
		}
		runs, _, err := e.tinvest.RunsByConnection(ctx, c.ID, 1, 0)
		if err != nil {
			return nil, nil, err
		}
		s.Failing = len(runs) > 0 && runs[0].Status == tinvest.RunFailed
		sync = append(sync, s)
		last, err := e.tinvest.LastReconcileByLink(ctx, c.ID)
		if err != nil {
			return nil, nil, err
		}
		links, err := e.tinvest.LinksByConnection(ctx, c.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, l := range links {
			run, ok := last[l.ID]
			if !ok || run.ReconcileStatus != tinvest.ReconcileMismatched || run.ReconciledAt == nil {
				continue
			}
			a := byID[l.AccountID]
			mismatches = append(mismatches, notify.Mismatch{AccountID: l.AccountID, Name: a.Name, Owner: a.OwnerUserID, RunID: run.ID, On: *run.ReconciledAt})
		}
	}
	return sync, mismatches, nil
}

// stale are the figure sources that stopped loading — the ones whose age
// leaves the screens' sums at old prices or rates.
func (e *events) stale(ctx context.Context, now time.Time) ([]notify.StaleSource, error) {
	list, err := Sources(ctx, e.pool)
	if err != nil {
		return nil, err
	}
	var out []notify.StaleSource
	for _, s := range list {
		if !slices.Contains(figureSources, s.Kind) || !s.Stale(now) {
			continue
		}
		out = append(out, notify.StaleSource{Kind: s.Kind, Name: figureNames[s.Kind], LastOK: s.LastSuccessAt})
	}
	return out, nil
}

// payoutReminders are the payouts that came in the last days and the
// dividends declared on the family's papers.
func (e *events) payoutReminders(ctx context.Context, spaceID uuid.UUID, today time.Time, byID map[uuid.UUID]account.Account) ([]notify.Reminder, error) {
	if e.payouts == nil {
		return nil, nil
	}
	checks, err := e.payouts.Received(ctx, spaceID, 4, payouts.Scope{})
	if err != nil {
		return nil, fmt.Errorf("payouts received: %w", err)
	}
	forecast, err := e.payouts.Forecast(ctx, spaceID, 3, payouts.Scope{})
	if err != nil {
		return nil, fmt.Errorf("payouts forecast: %w", err)
	}
	var ids []uuid.UUID
	for _, c := range checks {
		ids = append(ids, c.InstrumentID)
	}
	for _, ev := range forecast.Events {
		ids = append(ids, ev.InstrumentID)
	}
	papers, err := e.instruments.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	var arrived []notify.Arrived
	for _, c := range checks {
		if c.Status != payouts.Received || c.GotOn == nil {
			continue
		}
		arrived = append(arrived, notify.Arrived{
			AccountID: c.AccountID, Owner: byID[c.AccountID].OwnerUserID, Paper: papers[c.InstrumentID].Name,
			What: payoutWords[c.Kind], Got: c.Got, Currency: c.GotCurrency, On: *c.GotOn,
		})
	}
	return append(notify.PayoutReminders(arrived, today), notify.DeclaredReminders(declared(forecast, papers, byID), today)...), nil
}

// declared are the forecast's dividends with a record day, one per paper and
// record day: what reaches the family's accounts summed, in the payout's
// currency; the one member it is for when every account holding it is theirs.
func declared(f payouts.Forecast, papers map[uuid.UUID]instrument.Instrument, byID map[uuid.UUID]account.Account) []notify.Declared {
	type key struct {
		paper  uuid.UUID
		record time.Time
	}
	var order []key
	all := map[key]*notify.Declared{}
	owners := map[key]map[string]bool{}
	for _, ev := range f.Events {
		if ev.Kind != payouts.KindDividend || ev.RecordOn == nil || ev.PerUnit == nil {
			continue
		}
		k := key{ev.InstrumentID, *ev.RecordOn}
		d, ok := all[k]
		if !ok {
			d = &notify.Declared{
				InstrumentID: ev.InstrumentID, Paper: papers[ev.InstrumentID].Name, PerShare: *ev.PerUnit,
				Currency: ev.Currency, RecordOn: *ev.RecordOn, NetCurrency: ev.Currency,
			}
			// A payout's day is its record day when the payment's is not known.
			if on := ev.On; !on.Equal(*ev.RecordOn) {
				d.PayOn = &on
			}
			all[k], owners[k] = d, map[string]bool{}
			order = append(order, k)
		}
		if ev.Net != nil {
			sum := *ev.Net
			if d.Net != nil {
				sum += *d.Net
			}
			d.Net = &sum
		}
		who := "all"
		if o := byID[ev.AccountID].OwnerUserID; o != nil {
			who = o.String()
		}
		owners[k][who] = true
	}
	out := make([]notify.Declared, 0, len(order))
	for _, k := range order {
		d := all[k]
		if len(owners[k]) == 1 && !owners[k]["all"] {
			for who := range owners[k] {
				id := uuid.MustParse(who)
				d.Owner = &id
			}
		}
		out = append(out, *d)
	}
	return out
}
