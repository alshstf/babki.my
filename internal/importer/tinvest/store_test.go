package tinvest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/testdb"
)

// fixture is a migrated database holding one space, one babki account, one
// connection and one link between them — the smallest state in which the
// mirror can be exercised.
type fixture struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	store     *Store
	spaceID   uuid.UUID
	accountID uuid.UUID
	conn      Connection
	link      AccountLink
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()

	fam := family.NewStore(pool)
	u, err := fam.CreateUser(ctx, "alex", "Александр", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sp, err := fam.CreateSpaceWithOwner(ctx, "Семья", u.ID)
	if err != nil {
		t.Fatalf("create space: %v", err)
	}
	acc, err := account.NewStore(pool).Create(ctx, sp.ID, nil,
		"Т-Инвестиции", account.TypeBrokerage, "RUB", "Т-Банк")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	st := NewStore(pool)
	conn, err := st.CreateConnection(ctx, sp.ID, []byte("nonce||ciphertext"), "9f3a", StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	link, err := st.CreateLink(ctx, AccountLink{
		ConnectionID:      conn.ID,
		SpaceID:           sp.ID,
		AccountID:         acc.ID,
		BrokerAccountID:   "2000000001",
		BrokerAccountName: "Брокерский счёт",
		BrokerAccountType: "ACCOUNT_TYPE_TINKOFF",
	})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	return fixture{ctx: ctx, pool: pool, store: st, spaceID: sp.ID, accountID: acc.ID, conn: conn, link: link}
}

// secondLink adds another babki account and links it to the same connection.
func (f fixture) secondLink(t *testing.T) AccountLink {
	t.Helper()
	acc, err := account.NewStore(f.pool).Create(f.ctx, f.spaceID, nil,
		"ИИС", account.TypeBrokerage, "RUB", "Т-Банк")
	if err != nil {
		t.Fatalf("create second account: %v", err)
	}
	link, err := f.store.CreateLink(f.ctx, AccountLink{
		ConnectionID:      f.conn.ID,
		SpaceID:           f.spaceID,
		AccountID:         acc.ID,
		BrokerAccountID:   "2000000002",
		BrokerAccountName: "ИИС",
		BrokerAccountType: "ACCOUNT_TYPE_TINKOFF_IIS",
	})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	return link
}

// insertMirrorRow writes one mirror row straight to the table, for the one
// test that needs a state SyncMirror itself cannot produce. Everything else
// goes through SyncMirror.
func (f fixture) insertMirrorRow(t *testing.T, key string, firstSeen time.Time, disappearedAt *time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := f.pool.QueryRow(f.ctx, `
		INSERT INTO tinvest_operations_mirror (
			connection_id, link_id, broker_operation_id, op_type, state,
			occurred_at, currency, payment, raw, content_key,
			first_seen_at, last_confirmed_at, disappeared_at)
		VALUES ($1, $2, 'seeded', 'OPERATION_TYPE_INPUT', 'OPERATION_STATE_EXECUTED',
			now(), 'RUB', 0, '{}'::jsonb, $3, $4, $4, $5)
		RETURNING id`,
		f.conn.ID, f.link.ID, key, firstSeen, disappearedAt).Scan(&id)
	if err != nil {
		t.Fatalf("seed mirror row: %v", err)
	}
	return id
}

func TestCreateConnectionStoresWhatItWasGiven(t *testing.T) {
	f := newFixture(t)

	if f.conn.SpaceID != f.spaceID {
		t.Errorf("space = %s, want %s", f.conn.SpaceID, f.spaceID)
	}
	if f.conn.Status != StatusActive {
		t.Errorf("status = %q, want %q — the fixture asked for an active connection",
			f.conn.Status, StatusActive)
	}
	if string(f.conn.TokenCiphertext) != "nonce||ciphertext" {
		t.Errorf("ciphertext = %q", f.conn.TokenCiphertext)
	}
	if f.conn.TokenLast4 != "9f3a" {
		t.Errorf("last4 = %q, want 9f3a", f.conn.TokenLast4)
	}
	if f.conn.CreatedAt.IsZero() || f.conn.UpdatedAt.IsZero() {
		t.Errorf("timestamps = %s / %s", f.conn.CreatedAt, f.conn.UpdatedAt)
	}
}

// TestAConnectionIsCreatedWithTheStatusItWasAskedFor is the storage half of the
// parking Service.CreateConnection depends on. The column's own default is
// 'active', so an INSERT that left the status out would still write a row — a
// row the hourly dispatcher picks up — and only asking for the other status and
// reading it back says which of the two the statement actually used.
func TestAConnectionIsCreatedWithTheStatusItWasAskedFor(t *testing.T) {
	f := newFixture(t)

	parked, err := f.store.CreateConnection(f.ctx, f.spaceID, []byte("nonce||parked"), "0000", StatusDisabled)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if parked.Status != StatusDisabled {
		t.Errorf("status = %q, want %q", parked.Status, StatusDisabled)
	}

	// And the scheduler's own read agrees: the fixture's connection is active,
	// this one is not.
	active, err := f.store.ListActiveConnections(f.ctx)
	if err != nil {
		t.Fatalf("ListActiveConnections: %v", err)
	}
	if len(active) != 1 || active[0].ID != f.conn.ID {
		t.Errorf("the scheduler sees %+v, want only the active connection %s", active, f.conn.ID)
	}
}

func TestConnectionByIDIsScopedToTheSpace(t *testing.T) {
	f := newFixture(t)

	got, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatalf("ConnectionByID: %v", err)
	}
	if got.ID != f.conn.ID {
		t.Errorf("got connection %s, want %s", got.ID, f.conn.ID)
	}

	if _, err := f.store.ConnectionByID(f.ctx, uuid.New(), f.conn.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a stranger's space read the connection: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestListConnectionsIsScopedAndListActiveIsNot(t *testing.T) {
	f := newFixture(t)

	mine, err := f.store.ListConnections(f.ctx, f.spaceID)
	if err != nil {
		t.Fatalf("ListConnections: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != f.conn.ID {
		t.Fatalf("ListConnections returned %d connections, want just this space's one", len(mine))
	}
	strangers, err := f.store.ListConnections(f.ctx, uuid.New())
	if err != nil {
		t.Fatalf("ListConnections for another space: %v", err)
	}
	if len(strangers) != 0 {
		t.Errorf("another space sees %d connections, want 0", len(strangers))
	}

	// ListActiveConnections crosses spaces on purpose: it is what the
	// scheduler reads, and the scheduler has no space of its own.
	active, err := f.store.ListActiveConnections(f.ctx)
	if err != nil {
		t.Fatalf("ListActiveConnections: %v", err)
	}
	if len(active) != 1 || active[0].ID != f.conn.ID {
		t.Fatalf("ListActiveConnections returned %d, want 1", len(active))
	}

	if err := f.store.UpdateConnectionStatus(f.ctx, f.conn.ID, StatusTokenRevoked); err != nil {
		t.Fatalf("UpdateConnectionStatus: %v", err)
	}
	active, err = f.store.ListActiveConnections(f.ctx)
	if err != nil {
		t.Fatalf("ListActiveConnections: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("a revoked connection is still scheduled: %d active", len(active))
	}
	got, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatalf("ConnectionByID: %v", err)
	}
	if got.Status != StatusTokenRevoked {
		t.Errorf("status = %q, want %q", got.Status, StatusTokenRevoked)
	}
	if !got.UpdatedAt.After(f.conn.UpdatedAt) {
		t.Errorf("updated_at did not move: %s, was %s", got.UpdatedAt, f.conn.UpdatedAt)
	}
}

func TestUpdateConnectionTokenReplacesTheSecretAndNothingElse(t *testing.T) {
	f := newFixture(t)
	if err := f.store.UpdateConnectionStatus(f.ctx, f.conn.ID, StatusTokenRevoked); err != nil {
		t.Fatalf("UpdateConnectionStatus: %v", err)
	}

	if err := f.store.UpdateConnectionToken(f.ctx, f.spaceID, f.conn.ID, []byte("nonce||second"), "c4d1"); err != nil {
		t.Fatalf("UpdateConnectionToken: %v", err)
	}
	got, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatalf("ConnectionByID: %v", err)
	}
	if string(got.TokenCiphertext) != "nonce||second" || got.TokenLast4 != "c4d1" {
		t.Errorf("token = %q / %q, want nonce||second / c4d1", got.TokenCiphertext, got.TokenLast4)
	}
	if got.Status != StatusTokenRevoked {
		t.Errorf("status = %q; replacing the secret is not the decision to trust it again", got.Status)
	}

	if err := f.store.UpdateConnectionToken(f.ctx, uuid.New(), f.conn.ID, []byte("x"), "0000"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("another space replaced the token: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestDeleteConnectionTakesItsMirrorWithItAndLeavesTheAccount(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{op("op-1", "OPERATION_TYPE_INPUT", "", wireTime(t, "2026-01-09T05:00:00Z"), "RUB", 50000, 0, 0)},
		now); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}

	if err := f.store.DeleteConnection(f.ctx, uuid.New(), f.conn.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("another space deleted the connection: err = %v, want pgx.ErrNoRows", err)
	}
	if err := f.store.DeleteConnection(f.ctx, f.spaceID, f.conn.ID); err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}

	var mirrorRows, accounts int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM tinvest_operations_mirror`).Scan(&mirrorRows); err != nil {
		t.Fatalf("count mirror rows: %v", err)
	}
	if mirrorRows != 0 {
		t.Errorf("%d mirror rows outlived the connection they mirrored", mirrorRows)
	}
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM accounts WHERE id = $1`, f.accountID).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 1 {
		t.Errorf("the babki account went with the connection; it must not")
	}
}

func TestCreateLinkAndLinksByConnection(t *testing.T) {
	f := newFixture(t)
	other := f.secondLink(t)

	links, err := f.store.LinksByConnection(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("LinksByConnection: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("connection has %d links, want 2", len(links))
	}
	byBroker := map[string]AccountLink{}
	for _, l := range links {
		byBroker[l.BrokerAccountID] = l
	}
	if got := byBroker["2000000001"]; got.ID != f.link.ID || got.AccountID != f.accountID ||
		got.BrokerAccountName != "Брокерский счёт" || got.BrokerAccountType != "ACCOUNT_TYPE_TINKOFF" {
		t.Errorf("link = %+v", got)
	}
	if got := byBroker["2000000002"]; got.ID != other.ID {
		t.Errorf("second link = %+v", got)
	}
	if got := byBroker["2000000001"]; got.OpenedOn != nil {
		t.Errorf("opened on %v, want nothing — none was given", got.OpenedOn)
	}

	opened := time.Date(2019, 4, 1, 0, 0, 0, 0, time.UTC)
	acc, err := account.NewStore(f.pool).Create(f.ctx, f.spaceID, nil,
		"Третий", account.TypeBrokerage, "RUB", "Т-Банк")
	if err != nil {
		t.Fatalf("create third account: %v", err)
	}
	third, err := f.store.CreateLink(f.ctx, AccountLink{
		ConnectionID: f.conn.ID, SpaceID: f.spaceID, AccountID: acc.ID,
		BrokerAccountID: "2000000003", BrokerAccountName: "Третий",
		BrokerAccountType: "ACCOUNT_TYPE_TINKOFF", OpenedOn: &opened,
	})
	if err != nil {
		t.Fatalf("CreateLink with an opening date: %v", err)
	}
	if third.OpenedOn == nil || !third.OpenedOn.Equal(opened) {
		t.Errorf("opened on %v, want %s", third.OpenedOn, opened)
	}
}

// A link names three things — a space, a connection and a babki account — and
// the space it names has to be the space the other two are in. Neither of
// those two carries the space it belongs to on the argument, so nothing but
// this check stands between a mistaken caller and a link that files one
// space's broker operations into another's account.
func TestCreateLinkRefusesToCrossSpaces(t *testing.T) {
	f := newFixture(t)

	fam := family.NewStore(f.pool)
	stranger, err := fam.CreateUser(f.ctx, "petr", "Пётр", "hash")
	if err != nil {
		t.Fatalf("create the other user: %v", err)
	}
	otherSpace, err := fam.CreateSpaceWithOwner(f.ctx, "Чужая семья", stranger.ID)
	if err != nil {
		t.Fatalf("create the other space: %v", err)
	}
	otherConn, err := f.store.CreateConnection(f.ctx, otherSpace.ID, []byte("nonce||other"), "0000", StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection in the other space: %v", err)
	}
	otherAccount, err := account.NewStore(f.pool).Create(f.ctx, otherSpace.ID, nil,
		"Чужой счёт", account.TypeBrokerage, "RUB", "Т-Банк")
	if err != nil {
		t.Fatalf("create the other account: %v", err)
	}
	mine, err := account.NewStore(f.pool).Create(f.ctx, f.spaceID, nil,
		"Второй", account.TypeBrokerage, "RUB", "Т-Банк")
	if err != nil {
		t.Fatalf("create a second account of my own: %v", err)
	}

	// The connection belongs to the other space.
	if _, err := f.store.CreateLink(f.ctx, AccountLink{
		ConnectionID: otherConn.ID, SpaceID: f.spaceID, AccountID: mine.ID,
		BrokerAccountID: "2000000009", BrokerAccountName: "Чужое подключение",
		BrokerAccountType: "ACCOUNT_TYPE_TINKOFF",
	}); !errors.Is(err, ErrLinkOutsideSpace) {
		t.Errorf("a connection of another space was linked: err = %v, want ErrLinkOutsideSpace", err)
	}

	// The account belongs to the other space.
	if _, err := f.store.CreateLink(f.ctx, AccountLink{
		ConnectionID: f.conn.ID, SpaceID: f.spaceID, AccountID: otherAccount.ID,
		BrokerAccountID: "2000000010", BrokerAccountName: "Чужой счёт",
		BrokerAccountType: "ACCOUNT_TYPE_TINKOFF",
	}); !errors.Is(err, ErrLinkOutsideSpace) {
		t.Errorf("an account of another space was linked: err = %v, want ErrLinkOutsideSpace", err)
	}

	links, err := f.store.LinksByConnection(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("LinksByConnection: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("connection has %d links, want the 1 the fixture made", len(links))
	}
}

func TestRunsRecordTheOutcomeAndPaginate(t *testing.T) {
	f := newFixture(t)

	if at, err := f.store.LastSuccessfulSyncAt(f.ctx, f.conn.ID); err != nil || at != nil {
		t.Fatalf("LastSuccessfulSyncAt before any run = %v, %v; want nil, nil", at, err)
	}

	run, err := f.store.StartRun(f.ctx, f.conn.ID, f.link.ID, TriggerInitial)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if run.Status != RunRunning || run.Trigger != TriggerInitial {
		t.Errorf("run = %q / %q, want %q / %q", run.Status, run.Trigger, RunRunning, TriggerInitial)
	}
	if run.FinishedAt != nil {
		t.Errorf("a run that just started is finished at %s", run.FinishedAt)
	}
	if run.ReconcileStatus != ReconcileNotChecked {
		t.Errorf("reconcile status = %q, want %q", run.ReconcileStatus, ReconcileNotChecked)
	}

	if err := f.store.FinishRun(f.ctx, run.ID, RunOutcome{
		Status: RunOK, ReadCount: 120, AddedCount: 7, DisappearedCount: 1, UnparsedCount: 2,
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	runs, hasMore, err := f.store.RunsByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("RunsByConnection: %v", err)
	}
	if len(runs) != 1 || hasMore {
		t.Fatalf("RunsByConnection returned %d runs, hasMore %v; want 1, false", len(runs), hasMore)
	}
	got := runs[0]
	if got.Status != RunOK || got.ReadCount != 120 || got.AddedCount != 7 ||
		got.DisappearedCount != 1 || got.UnparsedCount != 2 || got.Error != "" {
		t.Errorf("finished run = %+v", got)
	}
	if got.FinishedAt == nil {
		t.Errorf("a finished run has no finishing time")
	}

	// "Last successful sync" is the START of that run, not its finish. It is
	// what the owner is shown as "the import last worked at"; nothing bounds a
	// fetch by it, because SyncMirror is given the whole history every time.
	at, err := f.store.LastSuccessfulSyncAt(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("LastSuccessfulSyncAt: %v", err)
	}
	if at == nil || !at.Equal(run.StartedAt) {
		t.Errorf("LastSuccessfulSyncAt = %v, want the run's start %s", at, run.StartedAt)
	}

	failed, err := f.store.StartRun(f.ctx, f.conn.ID, f.link.ID, TriggerSchedule)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := f.store.FinishRun(f.ctx, failed.ID, RunOutcome{
		Status: RunFailed, Error: "брокер ответил 429",
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	at, err = f.store.LastSuccessfulSyncAt(f.ctx, f.conn.ID)
	if err != nil {
		t.Fatalf("LastSuccessfulSyncAt: %v", err)
	}
	if at == nil || !at.Equal(run.StartedAt) {
		t.Errorf("a failed run moved LastSuccessfulSyncAt to %v, want %s", at, run.StartedAt)
	}

	// Newest first, and the second page is announced by fetching one row
	// past it rather than by comparing lengths.
	page, hasMore, err := f.store.RunsByConnection(f.ctx, f.conn.ID, 1, 0)
	if err != nil {
		t.Fatalf("RunsByConnection: %v", err)
	}
	if len(page) != 1 || !hasMore {
		t.Fatalf("first page = %d runs, hasMore %v; want 1, true", len(page), hasMore)
	}
	if page[0].ID != failed.ID {
		t.Errorf("first page starts with %s, want the newest run %s", page[0].ID, failed.ID)
	}
	if page[0].Error != "брокер ответил 429" {
		t.Errorf("error = %q", page[0].Error)
	}
	page, hasMore, err = f.store.RunsByConnection(f.ctx, f.conn.ID, 1, 1)
	if err != nil {
		t.Fatalf("RunsByConnection: %v", err)
	}
	if len(page) != 1 || hasMore || page[0].ID != run.ID {
		t.Fatalf("second page = %d runs, hasMore %v", len(page), hasMore)
	}

	if _, _, err := f.store.RunsByConnection(f.ctx, f.conn.ID, 0, 0); err == nil {
		t.Errorf("a limit of zero was accepted; it asks for the probe row alone")
	}
}

func TestFinishRunNamesARunThatIsNotThere(t *testing.T) {
	f := newFixture(t)
	if err := f.store.FinishRun(f.ctx, uuid.New(), RunOutcome{Status: RunOK}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("FinishRun on an unknown run = %v, want pgx.ErrNoRows", err)
	}
}

func TestUnparsedRowsAreListedSetAndCleared(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")
	items := []OperationItem{
		op("op-1", "OPERATION_TYPE_FUTURES", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -100, 0, 1),
		op("op-2", "OPERATION_TYPE_BUY", "uid-1", wireTime(t, "2026-03-15T07:30:15Z"), "RUB", -200, 0, 1),
		op("op-3", "OPERATION_TYPE_REPO", "uid-1", wireTime(t, "2026-03-16T07:30:15Z"), "RUB", -300, 0, 1),
	}
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, items, now); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	byPayment := rowsByPayment(t, f)

	rows, hasMore, err := f.store.UnparsedByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	if len(rows) != 0 || hasMore {
		t.Fatalf("a freshly synced mirror lists %d unparsed rows, want 0", len(rows))
	}

	if err := f.store.SetUnparsedReasons(f.ctx, map[uuid.UUID]string{
		byPayment["-100"].ID: "unknown_type",
		byPayment["-300"].ID: "unknown_type",
	}); err != nil {
		t.Fatalf("SetUnparsedReasons: %v", err)
	}

	rows, hasMore, err = f.store.UnparsedByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	if len(rows) != 2 || hasMore {
		t.Fatalf("UnparsedByConnection returned %d rows, hasMore %v; want 2, false", len(rows), hasMore)
	}
	// Newest first.
	if rows[0].BrokerOperationID != "op-3" || rows[1].BrokerOperationID != "op-1" {
		t.Errorf("order = %q, %q; want op-3, op-1", rows[0].BrokerOperationID, rows[1].BrokerOperationID)
	}
	for _, r := range rows {
		if r.UnparsedReason != "unknown_type" {
			t.Errorf("%s: reason = %q", r.BrokerOperationID, r.UnparsedReason)
		}
	}

	page, hasMore, err := f.store.UnparsedByConnection(f.ctx, f.conn.ID, 1, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	if len(page) != 1 || !hasMore {
		t.Fatalf("first page = %d rows, hasMore %v; want 1, true", len(page), hasMore)
	}

	// An empty reason clears the mark.
	if err := f.store.SetUnparsedReasons(f.ctx, map[uuid.UUID]string{byPayment["-100"].ID: ""}); err != nil {
		t.Fatalf("SetUnparsedReasons clearing: %v", err)
	}
	rows, _, err = f.store.UnparsedByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	if len(rows) != 1 || rows[0].BrokerOperationID != "op-3" {
		t.Errorf("after clearing, %d rows remain unparsed", len(rows))
	}

	if err := f.store.SetUnparsedReasons(f.ctx, nil); err != nil {
		t.Errorf("SetUnparsedReasons(nil) = %v, want nil", err)
	}
	if _, _, err := f.store.UnparsedByConnection(f.ctx, f.conn.ID, 0, 0); err == nil {
		t.Errorf("a limit of zero was accepted")
	}
}

// The ids handed to SetUnparsedReasons were read from this very table a
// moment ago. One that no longer matches a row means the mirror moved under
// the caller, and writing the part that still matches would leave a projection
// half-marked.
func TestSetUnparsedReasonsRefusesAnIDThatIsNotThere(t *testing.T) {
	f := newFixture(t)
	now := wireTime(t, "2026-03-16T00:00:00Z")
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{op("op-1", "OPERATION_TYPE_FUTURES", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -100, 0, 1)},
		now); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	real := rowsByPayment(t, f)["-100"].ID

	err := f.store.SetUnparsedReasons(f.ctx, map[uuid.UUID]string{
		real:       "unknown_type",
		uuid.New(): "unknown_type",
	})
	if !errors.Is(err, ErrUnparsedRowsMissing) {
		t.Fatalf("SetUnparsedReasons = %v, want ErrUnparsedRowsMissing", err)
	}
	rows, _, err := f.store.UnparsedByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d rows were marked anyway", len(rows))
	}
}

// A row nobody could read stays on the list after the broker stops returning
// it: it is still something this program could not read, and the row says for
// itself that the broker has dropped it.
func TestUnparsedByConnectionStillListsARowTheBrokerDropped(t *testing.T) {
	f := newFixture(t)
	first := wireTime(t, "2026-03-16T00:00:00Z")
	second := wireTime(t, "2026-03-16T01:00:00Z")
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link,
		[]OperationItem{op("op-1", "OPERATION_TYPE_FUTURES", "uid-1", wireTime(t, "2026-03-14T07:30:15Z"), "RUB", -100, 0, 1)},
		first); err != nil {
		t.Fatalf("SyncMirror: %v", err)
	}
	if err := f.store.SetUnparsedReasons(f.ctx, map[uuid.UUID]string{
		rowsByPayment(t, f)["-100"].ID: "unknown_type",
	}); err != nil {
		t.Fatalf("SetUnparsedReasons: %v", err)
	}
	if _, err := f.store.SyncMirror(f.ctx, f.conn.ID, f.link, nil, second); err != nil {
		t.Fatalf("second SyncMirror: %v", err)
	}

	rows, _, err := f.store.UnparsedByConnection(f.ctx, f.conn.ID, 10, 0)
	if err != nil {
		t.Fatalf("UnparsedByConnection: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d unparsed rows, want 1", len(rows))
	}
	if rows[0].DisappearedAt == nil {
		t.Errorf("the row does not say the broker dropped it")
	}
}
