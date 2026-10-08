package corporateaction

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
)

// RecordKnownConversionsArgs writes the corporate actions this program knows
// into the registry: the redomiciliations (decision Р-19) and the funds' spin-offs
// of their blocked assets. A broker reports no operation for either, so a holder's
// journal never gets the new paper.
type RecordKnownConversionsArgs struct{}

func (RecordKnownConversionsArgs) Kind() string { return "corporateaction.record_known_conversions" }

// knownNote is what each known conversion says about itself.
const knownNote = "Мосбиржа заменила расписки, учтённые в российских депозитариях, на акции российской компании один к одному"

// KnownConversions are the depositary receipts the exchange replaced by the
// Russian company's ordinary shares in its trading system, under the same
// trading code: every holding carried over one for one on the stated day, with
// nobody applying. Each is the exchange's own notice «Об изменении параметров
// ценных бумаг» (SourceRef), read on 2026-10-07.
//
// Only such automatic replacements are listed. An exchange that holders had to
// apply for (Yandex 2024, HeadHunter 2024, Cian 2025, Fix Price 2025) or a
// voluntary conversion (Lenta 2021, five receipts for a share) happened on each
// holder's own day, or not at all, and is recorded by hand.
var KnownConversions = []Event{
	knownConversion("US5603172082", "RU000A106YF0", "2023-09-27", "https://www.moex.com/n64257"), // VK
	knownConversion("US91085A2033", "RU000A107JE2", "2024-01-03", "https://www.moex.com/n66583"), // ЮМГ
	knownConversion("US87238U2033", "RU000A107UL4", "2024-02-27", "https://www.moex.com/n67851"), // ТКС Холдинг
	knownConversion("US55279C2008", "RU000A108KL3", "2024-05-30", "https://www.moex.com/n69805"), // МД Медикал Груп
	knownConversion("US29760G1031", "RU000A10C1L6", "2025-07-09", "https://www.moex.com/n91843"), // Эталон Груп
	knownConversion("US69269L1044", "RU000A10CW95", "2025-09-29", "https://www.moex.com/n93976"), // Озон
}

// KnownSpinOff is a spin-off this program knows, with the catalog row of the
// paper it produces: nothing arrives by an operation to catalogue the paper, and
// without the row the event would wait for good (NotCountedResultMissing).
type KnownSpinOff struct {
	Event
	Paper instrument.Instrument
}

// fundNote is what each known fund spin-off says about itself.
const fundNote = "Т-Капитал выделил заблокированные активы фонда в отдельный закрытый фонд: " +
	"владелец получил пай нового фонда на каждый свой пай; стоимость покупки остаётся на исходном фонде (решение Р-16)"

// KnownSpinOffs are T-Capital's exchange-traded funds whose blocked assets went
// into a closed fund of their own (Federal Law 319-FZ, art. 5.4). Each holder on
// the list date got as many units of the new fund as they held of the old (the
// new fund's rules, §52), so the event applies from the next day. SourceRef is
// the management company's notice of the list date, read on 2026-10-08; the new
// funds' tickers and names are the broker's.
var KnownSpinOffs = []KnownSpinOff{
	knownFundSpinOff("RU000A101X68", "RU000A1071G8", "TECH2", "Тинькофф Технологии заблокированные активы", "2023-10-16",
		"https://cdn.t-capital-funds.ru/static/documents/2ef634b8-3cd3-49c5-b815-30a49bad27ad.pdf"),
	knownFundSpinOff("RU000A102EQ8", "RU000A1071F0", "TSPX2", "Тинькофф США 500 заблокированные активы", "2023-10-16",
		"https://cdn.t-capital-funds.ru/static/documents/77740bf1-c4c7-468f-acbc-6f1dbbb38faa.pdf"),
	knownFundSpinOff("RU000A1011S9", "RU000A109JM1", "TUSD2", "Заблокированные активы «Вечный портфель USD»", "2024-09-18",
		"https://cdn.t-capital-funds.ru/static/documents/soobshenie-o-sostavlenii-spiska-vladelcev-tusd.pdf"),
}

func knownFundSpinOff(fund, blocked, ticker, name, listedOn, notice string) KnownSpinOff {
	day, err := time.Parse(time.DateOnly, listedOn)
	if err != nil {
		panic("corporateaction: known spin-off " + fund + ": " + err.Error())
	}
	none := decimal.Zero
	return KnownSpinOff{
		Event: Event{
			Kind: KindSpinOff, ISIN: fund, ResultISIN: blocked, EffectiveOn: day.AddDate(0, 0, 1),
			RatioFrom: 1, RatioTo: 1, BasisShare: &none, Source: SourceKnown, SourceRef: notice, Note: fundNote,
		},
		Paper: instrument.Instrument{Type: instrument.TypeETF, Name: name, Ticker: ticker, ISIN: blocked, Currency: "RUB"},
	}
}

func knownConversion(receipt, share, on, notice string) Event {
	day, err := time.Parse(time.DateOnly, on)
	if err != nil {
		panic("corporateaction: known conversion " + receipt + ": " + err.Error())
	}
	return Event{
		Kind: KindConversion, ISIN: receipt, ResultISIN: share, EffectiveOn: day,
		RatioFrom: 1, RatioTo: 1, Source: SourceKnown, SourceRef: notice, Note: knownNote,
	}
}

// paperMaker is the catalog the worker files a known spin-off's new paper in.
type paperMaker interface {
	ByISIN(ctx context.Context, isin string) (instrument.Instrument, error)
	Create(ctx context.Context, inst instrument.Instrument) (instrument.Instrument, error)
}

type recordKnownConversionsWorker struct {
	river.WorkerDefaults[RecordKnownConversionsArgs]
	store        *Store
	materializer *Materializer
	papers       paperMaker
	log          *slog.Logger
}

// NewRecordKnownConversionsWorker builds the worker.
func NewRecordKnownConversionsWorker(store *Store, materializer *Materializer, papers paperMaker, log *slog.Logger,
) river.Worker[RecordKnownConversionsArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &recordKnownConversionsWorker{store: store, materializer: materializer, papers: papers, log: log}
}

func (w *recordKnownConversionsWorker) Timeout(*river.Job[RecordKnownConversionsArgs]) time.Duration {
	return refreshTimeout
}

// Work records every known event and brings the holders into line. A person's
// own record of the same day is kept.
func (w *recordKnownConversionsWorker) Work(ctx context.Context, _ *river.Job[RecordKnownConversionsArgs]) error {
	var (
		failed []error
		totals Stats
		kept   int
	)
	known := slices.Clone(KnownConversions)
	for _, s := range KnownSpinOffs {
		if err := w.catalogue(ctx, s.Paper); err != nil {
			return err
		}
		known = append(known, s.Event)
	}
	for _, e := range known {
		_, written, err := w.store.Upsert(ctx, e)
		if err != nil {
			return err
		}
		if !written {
			kept++
			continue
		}
		s, err := w.materializer.ForISIN(ctx, e.ISIN)
		totals.add(s)
		if err != nil {
			w.log.Error("corporateaction: carrying a known conversion into the journals failed", "isin", e.ISIN, "err", err)
			failed = append(failed, err)
		}
	}
	w.materializer.RequestRecheck(ctx, totals)
	w.log.Info("corporateaction: recorded the known corporate actions",
		"known", len(known), "left_to_other_records", kept,
		"journal_rows_added", totals.Added, "journal_rows_removed", totals.Removed)
	return errors.Join(failed...)
}

// catalogue files a known spin-off's new paper unless the catalog has it. A
// ticker another row holds leaves it out, and the event waits for a person.
func (w *recordKnownConversionsWorker) catalogue(ctx context.Context, paper instrument.Instrument) error {
	_, err := w.papers.ByISIN(ctx, paper.ISIN)
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	created, err := w.papers.Create(ctx, paper)
	switch {
	case errors.Is(err, instrument.ErrISINTaken):
		// Another writer filed it first.
		return nil
	case errors.Is(err, instrument.ErrTickerTaken):
		w.log.Warn("corporateaction: a known spin-off's new paper was not catalogued: its ticker is another row's",
			"isin", paper.ISIN, "ticker", paper.Ticker)
		return nil
	case err != nil:
		return err
	}
	// Info, as the importer's: a row added to the shared catalog.
	w.log.Info("corporateaction: catalogued a known spin-off's new paper",
		"instrument_id", created.ID, "isin", created.ISIN, "ticker", created.Ticker)
	return nil
}
