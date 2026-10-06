package corporateaction

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// RecordKnownConversionsArgs writes the redomiciliations this program knows
// into the registry (decision Р-19). A broker forgets a receipt it once traded
// and reports no operation for its replacement, so a holder's later sale of the
// new share finds nothing to sell.
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

type recordKnownConversionsWorker struct {
	river.WorkerDefaults[RecordKnownConversionsArgs]
	store        *Store
	materializer *Materializer
	log          *slog.Logger
}

// NewRecordKnownConversionsWorker builds the worker.
func NewRecordKnownConversionsWorker(store *Store, materializer *Materializer, log *slog.Logger,
) river.Worker[RecordKnownConversionsArgs] {
	if log == nil {
		log = slog.Default()
	}
	return &recordKnownConversionsWorker{store: store, materializer: materializer, log: log}
}

func (w *recordKnownConversionsWorker) Timeout(*river.Job[RecordKnownConversionsArgs]) time.Duration {
	return refreshTimeout
}

// Work records every known conversion and brings the receipts' holders into
// line. A person's own record of the same day is kept.
func (w *recordKnownConversionsWorker) Work(ctx context.Context, _ *river.Job[RecordKnownConversionsArgs]) error {
	var (
		failed []error
		totals Stats
		kept   int
	)
	for _, e := range KnownConversions {
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
	w.log.Info("corporateaction: recorded the known conversions",
		"known", len(KnownConversions), "left_to_other_records", kept,
		"journal_rows_added", totals.Added, "journal_rows_removed", totals.Removed)
	return errors.Join(failed...)
}
