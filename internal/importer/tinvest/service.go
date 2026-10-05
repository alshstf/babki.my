package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/secretbox"
)

// ErrTokenRejected means the broker was reached and refused the token: paste a
// new one. It is kept apart from ErrBrokerUnreachable (wait) by status code, the
// only difference a client may branch on.
var ErrTokenRejected = errors.New("tinvest: the broker refused this token")

// ErrBrokerUnreachable means the broker call produced no usable answer:
// network, gateway error, a rate limit that did not clear, an unparsable
// response. Anything but a refused token.
var ErrBrokerUnreachable = errors.New("tinvest: the broker could not be reached")

// ErrConnectionNotActive means a sync was asked for on a connection the
// scheduler skips (switched off, or waiting for a new token); a queued job would
// be dropped on sight.
var ErrConnectionNotActive = errors.New("tinvest: the connection is not active, so it cannot be synced")

// ErrBrokerAccountAlreadyLinked means a picked broker account is already
// imported by a connection of this space; two links would import the same
// operations twice.
var ErrBrokerAccountAlreadyLinked = errors.New("tinvest: that broker account is already imported by another connection of this space")

// ErrBrokerAccountNotImportable means a picked account is not visible to the
// token or not a kind this program imports (importableAccountTypes): such a link
// would sync forever and produce nothing. A 422, not ErrTokenRejected's 400 (see
// writeError).
var ErrBrokerAccountNotImportable = errors.New("tinvest: the token does not see that broker account, or it is not of a kind this program imports")

// importableAccountTypes are the account kinds offered for import: brokerage
// and ИИС, as the gateway's enum strings. The «инвесткопилка», card and savings
// accounts a token also reaches carry operations the projection has no rules
// for.
var importableAccountTypes = map[string]bool{
	"ACCOUNT_TYPE_TINKOFF":     true,
	"ACCOUNT_TYPE_TINKOFF_IIS": true,
}

// importedAccountCurrency and importedAccountInstitution are what an imported
// account is created as. The currency matters: the reconciliation writes the
// broker's rouble balance onto the account and refuses an account in any other
// currency (ErrAccountNotInRubles).
const (
	importedAccountCurrency    = "RUB"
	importedAccountInstitution = "Т-Банк"
)

// tokenLast4Len is how much of the token is shown again; a shorter token
// gives a shorter tail.
const tokenLast4Len = 4

// tokenLast4 is the published tail of the token, cut by runes so invalid
// UTF-8 never reaches JSON.
func tokenLast4(token string) string {
	r := []rune(token)
	if len(r) <= tokenLast4Len {
		return string(r)
	}
	return string(r[len(r)-tokenLast4Len:])
}

// accountCreator is the account store as connection setup uses it.
type accountCreator interface {
	Create(ctx context.Context, spaceID uuid.UUID, ownerUserID *uuid.UUID,
		name string, t account.Type, currency, institution string) (account.Account, error)
}

// AccountPick is one broker account the owner chose to import, and what to call
// the babki account it is imported into.
type AccountPick struct {
	BrokerAccountID string
	AccountName     string
}

// ConnectionUpdate is a partial change; a nil field is left alone, so an
// accidental "" is refused rather than read as "leave it".
type ConnectionUpdate struct {
	Token  *string
	Status *ConnectionStatus
}

// ConnectionView is a connection with what a screen shows: its row, its linked
// accounts, its last successful sync and the last check of each linked account.
// LastReconcileByLink is per link because a verdict is per account; a missing
// link was never checked. LastSuccessfulSyncAt is read separately: a run can
// succeed without reconciling.
type ConnectionView struct {
	Connection           Connection
	Links                []AccountLink
	LastSuccessfulSyncAt *time.Time
	LastReconcileByLink  map[uuid.UUID]SyncRun
	// CurrencyTradesUnparsedByLink is how many currency trades of each link are
	// not imported, which tells a cash difference that cannot close from one that
	// can. Absent means none.
	CurrencyTradesUnparsedByLink map[uuid.UUID]int
}

// journalWriter is the journal's own door: a manual operation written in place
// of the imported rows it accounts for, and one deleted. Through the operation
// service, so a hand entry here is validated and replayed like one on the
// journal screen. CreateReplacing because the explained rows may be rows this
// importer booked (a fund's partial redemption read as a transfer_out), and
// removing them in a separate transaction would leave a window with neither
// reading of the event.
type journalWriter interface {
	CreateReplacing(ctx context.Context, spaceID uuid.UUID, op operation.Operation, replace []uuid.UUID) (
		operation.Operation, error)
	Delete(ctx context.Context, spaceID, id uuid.UUID) error
}

// Service is the request path of the T-Invest importer: everything a person does
// to a connection, as opposed to what the background worker does. Every method is
// owner-only and checks it first, as family.Service.UpdateSpace does, so no route
// or later caller can get around it.
type Service struct {
	store    *Store
	accounts accountCreator
	journal  journalWriter
	// entries reads back this importer's rows, the same interface the rebuild
	// reads through.
	entries   journalReader
	box       *secretbox.Box
	newClient clientFactory
	inserter  jobInserter
	log       *slog.Logger
}

func NewService(store *Store, accounts accountCreator, journal journalWriter, entries journalReader,
	box *secretbox.Box, newClient clientFactory, inserter jobInserter, log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: store, accounts: accounts, journal: journal, entries: entries, box: box,
		newClient: newClient, inserter: inserter, log: log,
	}
}

// requireOwner is the first statement of every exported method below.
func requireOwner(p family.Principal) error {
	if p.Role != family.RoleOwner {
		return family.ErrForbidden
	}
	return nil
}

// checkToken asks the broker which accounts a token sees and keeps the
// importable ones. The token is sent exactly as pasted; only emptiness is
// refused.
func (s *Service) checkToken(ctx context.Context, token string) ([]Account, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: token must not be empty", family.ErrValidation)
	}
	c, err := s.newClient(token)
	if err != nil {
		// Not ErrBrokerUnreachable: the broker was not asked yet. A client that
		// cannot be built is the instance's fault, a 500.
		return nil, fmt.Errorf("tinvest: build the broker client: %w", err)
	}
	accounts, err := c.GetAccounts(ctx)
	if errors.Is(err, ErrTokenInvalid) {
		return nil, ErrTokenRejected
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBrokerUnreachable, err)
	}
	out := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		if importableAccountTypes[a.Type] {
			out = append(out, a)
		}
	}
	return out, nil
}

// CheckToken lists the accounts a read-only token can see so the owner can
// pick. Nothing is stored.
func (s *Service) CheckToken(ctx context.Context, p family.Principal, token string) ([]Account, error) {
	if err := requireOwner(p); err != nil {
		return nil, err
	}
	return s.checkToken(ctx, token)
}

// CreateConnection stores the token, creates one babki account per pick, links
// them and queues the first import.
//
// Everything judgeable without the database (the request, the broker's answer,
// already-imported picks) is judged first, so a refusal writes nothing. The
// connection is created switched off and switched on only once its accounts and
// links exist, since an active connection is one the hourly scheduler syncs.
//
// Cleanup after a partial failure is best effort (undoConnection):
//
//  1. Write fails, cleanup succeeds: connection and links gone; the created
//     accounts stay (accounts are never deleted behind the owner's back) and can
//     be archived.
//  2. Write fails, cleanup fails: a half-built connection stays, switched off, so
//     nothing syncs from it; the owner can delete it.
//  3. Switching on fails: as 2.
//  4. Queueing the first sync fails and cleanup fails: a complete active
//     connection stays and the hourly run imports it as if the request had
//     succeeded. Queueing comes after the switch-on because the worker drops a
//     job for an inactive connection.
//
// The stores take no outside transaction, so one transaction is not available.
// The already-imported check is not race-proof and no constraint backs it; two
// simultaneous creations would both pass, which one button cannot cause.
func (s *Service) CreateConnection(ctx context.Context, p family.Principal,
	token string, picks []AccountPick,
) (ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return ConnectionView{}, err
	}
	if err := validatePicks(picks); err != nil {
		return ConnectionView{}, err
	}
	brokerAccounts, err := s.checkToken(ctx, token)
	if err != nil {
		return ConnectionView{}, err
	}
	byID := make(map[string]Account, len(brokerAccounts))
	for _, a := range brokerAccounts {
		byID[a.ID] = a
	}
	chosen := make([]Account, 0, len(picks))
	for _, pick := range picks {
		a, ok := byID[pick.BrokerAccountID]
		if !ok {
			return ConnectionView{}, fmt.Errorf("%w: %s", ErrBrokerAccountNotImportable, pick.BrokerAccountID)
		}
		chosen = append(chosen, a)
	}
	linked, err := s.linkedBrokerAccounts(ctx, p.SpaceID)
	if err != nil {
		return ConnectionView{}, err
	}
	for _, pick := range picks {
		if linked[pick.BrokerAccountID] {
			return ConnectionView{}, fmt.Errorf("%w: %s", ErrBrokerAccountAlreadyLinked, pick.BrokerAccountID)
		}
	}

	conn, err := s.store.CreateConnection(ctx, p.SpaceID, s.box.Seal([]byte(token)),
		tokenLast4(token), StatusDisabled)
	if err != nil {
		return ConnectionView{}, err
	}
	links := make([]AccountLink, 0, len(picks))
	for i, pick := range picks {
		// Shared (nil owner), like a hand-made brokerage account.
		acc, err := s.accounts.Create(ctx, p.SpaceID, nil, pick.AccountName,
			account.TypeBrokerage, importedAccountCurrency, importedAccountInstitution)
		if err != nil {
			return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
		}
		link, err := s.store.CreateLink(ctx, AccountLink{
			ConnectionID:      conn.ID,
			SpaceID:           p.SpaceID,
			AccountID:         acc.ID,
			BrokerAccountID:   pick.BrokerAccountID,
			BrokerAccountName: chosen[i].Name,
			BrokerAccountType: chosen[i].Type,
			OpenedOn:          chosen[i].OpenedOn,
		})
		if err != nil {
			return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
		}
		links = append(links, link)
	}

	// Read back, so the answer shows the stored status.
	if err := s.store.UpdateConnectionStatus(ctx, conn.ID, StatusActive); err != nil {
		return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
	}
	active, err := s.store.ConnectionByID(ctx, p.SpaceID, conn.ID)
	if err != nil {
		return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
	}

	// Through EnqueueSync, so every way of starting a sync shares one
	// uniqueness class.
	if _, err := EnqueueSync(ctx, s.inserter, conn.ID, TriggerInitial); err != nil {
		return ConnectionView{}, s.undoConnection(p.SpaceID, conn.ID, err)
	}
	return ConnectionView{Connection: active, Links: links}, nil
}

// undoConnection removes a connection whose setup failed and returns the
// original failure; the cleanup's own error is logged. Best effort, which is why
// the connection was created switched off. It runs on its own short context:
// the failure is often a cancelled request, whose context would cancel the
// cleanup too.
func (s *Service) undoConnection(spaceID, id uuid.UUID, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), undoTimeout)
	defer cancel()
	if err := s.store.DeleteConnection(ctx, spaceID, id); err != nil {
		s.log.Error("tinvest: removing a half-built connection failed", "connection", id, "err", err)
	}
	return cause
}

// undoTimeout bounds the cleanup above.
const undoTimeout = 5 * time.Second

// validatePicks judges the request's shape before the broker or database is
// asked.
func validatePicks(picks []AccountPick) error {
	if len(picks) == 0 {
		return fmt.Errorf("%w: pick at least one broker account to import", family.ErrValidation)
	}
	seen := make(map[string]bool, len(picks))
	for _, p := range picks {
		if p.BrokerAccountID == "" {
			return fmt.Errorf("%w: broker_account_id must not be empty", family.ErrValidation)
		}
		if p.AccountName == "" {
			return fmt.Errorf("%w: account_name must not be empty", family.ErrValidation)
		}
		if seen[p.BrokerAccountID] {
			// Caught here rather than by the unique index after an account was
			// already created.
			return fmt.Errorf("%w: broker account %s is named twice", family.ErrValidation, p.BrokerAccountID)
		}
		seen[p.BrokerAccountID] = true
	}
	return nil
}

// linkedBrokerAccounts is every broker account imported by any connection of
// this space, assembled from the existing reads (a handful of rows) rather than a
// third query about what a link is.
func (s *Service) linkedBrokerAccounts(ctx context.Context, spaceID uuid.UUID) (map[string]bool, error) {
	conns, err := s.store.ListConnections(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range conns {
		links, err := s.store.LinksByConnection(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			out[l.BrokerAccountID] = true
		}
	}
	return out, nil
}

// UpdateConnection replaces the token, switches the connection on or off, or
// both. A new token is proved with the broker before it is stored, which also
// brings a connection back from token_revoked; an explicit status in the same
// request wins.
func (s *Service) UpdateConnection(ctx context.Context, p family.Principal,
	id uuid.UUID, upd ConnectionUpdate,
) (ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return ConnectionView{}, err
	}
	if upd.Token == nil && upd.Status == nil {
		return ConnectionView{}, fmt.Errorf("%w: nothing to update: give token, status or both", family.ErrValidation)
	}
	if upd.Status != nil && *upd.Status != StatusActive && *upd.Status != StatusDisabled {
		// token_revoked is this server's record of the broker's answer; a client
		// may not set it.
		return ConnectionView{}, fmt.Errorf("%w: status must be %q or %q",
			family.ErrValidation, StatusActive, StatusDisabled)
	}
	if _, err := s.store.ConnectionByID(ctx, p.SpaceID, id); err != nil {
		return ConnectionView{}, err
	}

	status := upd.Status
	if upd.Token != nil {
		if _, err := s.checkToken(ctx, *upd.Token); err != nil {
			return ConnectionView{}, err
		}
		if err := s.store.UpdateConnectionToken(ctx, p.SpaceID, id,
			s.box.Seal([]byte(*upd.Token)), tokenLast4(*upd.Token)); err != nil {
			return ConnectionView{}, err
		}
		if status == nil {
			active := StatusActive
			status = &active
		}
	}
	if status != nil {
		if err := s.store.UpdateConnectionStatus(ctx, id, *status); err != nil {
			return ConnectionView{}, err
		}
	}
	return s.Connection(ctx, p, id)
}

// DeleteConnection withdraws the authorization to read from the broker. The
// accounts and operations it created stay (see Store.DeleteConnection).
func (s *Service) DeleteConnection(ctx context.Context, p family.Principal, id uuid.UUID) error {
	if err := requireOwner(p); err != nil {
		return err
	}
	return s.store.DeleteConnection(ctx, p.SpaceID, id)
}

// TriggerSync queues a sync now and reports whether this request queued it.
// False means one was already queued, possibly waiting out a retry backoff of
// hours, not that one is running (see EnqueueSync).
func (s *Service) TriggerSync(ctx context.Context, p family.Principal, id uuid.UUID) (bool, error) {
	if err := requireOwner(p); err != nil {
		return false, err
	}
	conn, err := s.store.ConnectionByID(ctx, p.SpaceID, id)
	if err != nil {
		return false, err
	}
	if conn.Status != StatusActive {
		return false, fmt.Errorf("%w: it is %q", ErrConnectionNotActive, conn.Status)
	}
	res, err := EnqueueSync(ctx, s.inserter, id, TriggerManual)
	if err != nil {
		return false, fmt.Errorf("tinvest: queue a sync: %w", err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}

// ListConnections returns every connection of the caller's space, each with
// what a screen shows about it.
func (s *Service) ListConnections(ctx context.Context, p family.Principal) ([]ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return nil, err
	}
	conns, err := s.store.ListConnections(ctx, p.SpaceID)
	if err != nil {
		return nil, err
	}
	out := make([]ConnectionView, 0, len(conns))
	for _, c := range conns {
		view, err := s.view(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

// Connection returns one connection of the caller's space, or pgx.ErrNoRows
// (see Store.ConnectionByID).
func (s *Service) Connection(ctx context.Context, p family.Principal, id uuid.UUID) (ConnectionView, error) {
	if err := requireOwner(p); err != nil {
		return ConnectionView{}, err
	}
	conn, err := s.store.ConnectionByID(ctx, p.SpaceID, id)
	if err != nil {
		return ConnectionView{}, err
	}
	return s.view(ctx, conn)
}

func (s *Service) view(ctx context.Context, conn Connection) (ConnectionView, error) {
	links, err := s.store.LinksByConnection(ctx, conn.ID)
	if err != nil {
		return ConnectionView{}, err
	}
	lastSync, err := s.store.LastSuccessfulSyncAt(ctx, conn.ID)
	if err != nil {
		return ConnectionView{}, err
	}
	reconciles, err := s.store.LastReconcileByLink(ctx, conn.ID)
	if err != nil {
		return ConnectionView{}, err
	}
	currencyTrades, err := s.store.CurrencyTradesUnparsedByLink(ctx, conn.ID)
	if err != nil {
		return ConnectionView{}, err
	}
	return ConnectionView{
		Connection:                   conn,
		Links:                        links,
		LastSuccessfulSyncAt:         lastSync,
		LastReconcileByLink:          reconciles,
		CurrencyTradesUnparsedByLink: currencyTrades,
	}, nil
}

// Runs returns a page of the connection's sync log, newest first, and whether
// there is more. Reading the connection by the caller's space first is what keeps
// this off a stranger's log; Store.RunsByConnection checks no space.
func (s *Service) Runs(ctx context.Context, p family.Principal, id uuid.UUID, limit, offset int) (
	[]SyncRun, bool, error,
) {
	if err := requireOwner(p); err != nil {
		return nil, false, err
	}
	if _, err := s.store.ConnectionByID(ctx, p.SpaceID, id); err != nil {
		return nil, false, err
	}
	return s.store.RunsByConnection(ctx, id, limit, offset)
}

// Unparsed returns a page of the connection's unreadable operations, newest
// first, scoped like Runs.
func (s *Service) Unparsed(ctx context.Context, p family.Principal, id uuid.UUID, limit, offset int) (
	[]MirrorRow, bool, error,
) {
	if err := requireOwner(p); err != nil {
		return nil, false, err
	}
	if _, err := s.store.ConnectionByID(ctx, p.SpaceID, id); err != nil {
		return nil, false, err
	}
	return s.store.UnparsedByConnection(ctx, id, limit, offset)
}
