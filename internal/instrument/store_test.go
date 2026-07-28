package instrument_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/testdb"
)

func newStore(t *testing.T) (*instrument.Store, context.Context) {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return instrument.NewStore(pool), ctx
}

func TestInstrumentLifecycle(t *testing.T) {
	st, ctx := newStore(t)

	face := int64(1_000_00)
	faceCur := "RUB"
	bond, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4",
		ISIN: "RU000A1038V6", Currency: "RUB",
		FaceValueMinor: &face, FaceCurrency: &faceCur,
	})
	if err != nil {
		t.Fatalf("Create bond: %v", err)
	}
	if bond.ID.String() == "" || bond.FaceValueMinor == nil || *bond.FaceValueMinor != 1_000_00 {
		t.Fatalf("bond = %+v", bond)
	}

	share, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER",
		ISIN: "RU0009029540", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}

	// search by ticker fragment, case-insensitive
	found, err := st.Search(ctx, "sber", 10)
	if err != nil || len(found) != 1 || found[0].ID != share.ID {
		t.Fatalf("Search sber = %+v, %v", found, err)
	}
	// search by name fragment
	if found, _ = st.Search(ctx, "офз", 10); len(found) != 1 {
		t.Fatalf("Search офз = %+v", found)
	}
	// empty query returns all ordered by name
	if found, _ = st.Search(ctx, "", 10); len(found) != 2 {
		t.Fatalf("Search all = %d", len(found))
	}

	// update: freeze + rename ticker
	frozen := true
	newTicker := "SBERP"
	upd, err := st.Update(ctx, share.ID, instrument.Update{Frozen: &frozen, Ticker: &newTicker})
	if err != nil || !upd.Frozen || upd.Ticker != "SBERP" {
		t.Fatalf("Update = %+v, %v", upd, err)
	}

	// tri-state: clear face value on the bond
	var nilFace *int64
	var nilCur *string
	upd, err = st.Update(ctx, bond.ID, instrument.Update{FaceValueMinor: &nilFace, FaceCurrency: &nilCur})
	if err != nil || upd.FaceValueMinor != nil {
		t.Fatalf("clear face = %+v, %v", upd, err)
	}

	if _, err := st.ByID(ctx, share.ID); err != nil {
		t.Fatalf("ByID: %v", err)
	}
}

func TestListTradable(t *testing.T) {
	st, ctx := newStore(t)

	share, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}
	bond, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4", Currency: "RUB",
	})
	if err != nil {
		t.Fatalf("Create bond: %v", err)
	}
	etf, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeETF, Name: "FinEx USA", Ticker: "FXUS", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("Create etf: %v", err)
	}
	// no ticker -> excluded even though the type is tradable.
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Без тикера", Currency: "RUB",
	}); err != nil {
		t.Fatalf("Create tickerless: %v", err)
	}
	// non-tradable type with a ticker -> excluded.
	if _, err := st.Create(ctx, instrument.Instrument{
		Type: instrument.TypeCurrency, Name: "USD", Ticker: "USD000UTSTOM", Currency: "USD",
	}); err != nil {
		t.Fatalf("Create currency: %v", err)
	}

	got, err := st.ListTradable(ctx)
	if err != nil {
		t.Fatalf("ListTradable: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListTradable len = %d, want 3: %+v", len(got), got)
	}
	ids := map[uuid.UUID]bool{}
	for _, i := range got {
		if i.Ticker == "" {
			t.Fatalf("ListTradable returned tickerless instrument: %+v", i)
		}
		ids[i.ID] = true
	}
	for _, want := range []uuid.UUID{share.ID, bond.ID, etf.ID} {
		if !ids[want] {
			t.Fatalf("ListTradable missing %v", want)
		}
	}
}

func TestTypeValid(t *testing.T) {
	if !instrument.TypeShare.Valid() || instrument.Type("nope").Valid() {
		t.Error("Type.Valid broken")
	}
}
