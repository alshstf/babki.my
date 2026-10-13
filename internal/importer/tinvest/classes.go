package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/secretbox"
)

// A paper's country of risk and sector come from its passport at the broker
// (decision Р-46): the country where the business is, not the ISIN's — FinEx's
// American shares fund is Irish by ISIN, American by risk. The catalog is the
// instance's, so any active connection's token serves every space. A share the
// broker does not know, or every share when no connection is active, takes
// its sector from Yahoo, without a country.

// RefreshClassesArgs is the periodic job classifying the catalog's papers.
type RefreshClassesArgs struct{}

func (RefreshClassesArgs) Kind() string { return "tinvest.refresh_classes" }

// classesPerRun bounds one run: two calls a paper, well under the broker's
// limit on instrument calls a minute; a long catalog fills over several runs.
const classesPerRun = 60

// classesAge is how old a class may grow before it is asked again: a company
// changes its business rarely, a fund its focus never.
const classesAge = 30 * 24 * time.Hour

// SectorFeed names a share's sector by its ISIN; Yahoo's search does.
type SectorFeed interface {
	SectorFor(ctx context.Context, isin string) (string, bool, error)
}

type classStore interface {
	ToClassify(ctx context.Context, olderThan time.Time, limit int) ([]instrument.Instrument, error)
	SetClass(ctx context.Context, id uuid.UUID, c instrument.Class) error
}

type classesWorker struct {
	river.WorkerDefaults[RefreshClassesArgs]
	store     *Store
	classes   classStore
	sectors   SectorFeed
	box       *secretbox.Box
	newClient clientFactory
	log       *slog.Logger
	now       func() time.Time
}

// NewClassesWorker builds the worker that classifies papers by the broker's
// passports, and by sectors (nil for none) where those say nothing; nil now
// is time.Now.
func NewClassesWorker(store *Store, classes classStore, sectors SectorFeed, box *secretbox.Box, newClient clientFactory, log *slog.Logger, now func() time.Time) river.Worker[RefreshClassesArgs] {
	if now == nil {
		now = time.Now
	}
	return &classesWorker{store: store, classes: classes, sectors: sectors, box: box, newClient: newClient, log: log, now: now}
}

func (w *classesWorker) Timeout(*river.Job[RefreshClassesArgs]) time.Duration {
	return 5 * time.Minute
}

// Work classifies the papers due, through the first connection whose token
// the broker takes; a revoked token is marked and the next one tried; with no
// connection left, by sectors alone.
func (w *classesWorker) Work(ctx context.Context, _ *river.Job[RefreshClassesArgs]) error {
	papers, err := w.classes.ToClassify(ctx, w.now().Add(-classesAge), classesPerRun)
	if err != nil || len(papers) == 0 {
		return err
	}
	conns, err := w.store.ListActiveConnections(ctx)
	if err != nil {
		return err
	}
	for _, conn := range conns {
		token, err := w.box.Open(conn.TokenCiphertext)
		if err != nil {
			w.log.Error("tinvest: open a connection's token for classes failed", "connection_id", conn.ID, "err", err)
			continue
		}
		client, err := w.newClient(string(token))
		if err != nil {
			return err
		}
		n, err := w.classify(ctx, client, papers)
		if errors.Is(err, ErrTokenInvalid) {
			w.log.Warn("tinvest: the broker rejected this connection's token while classifying papers", "connection_id", conn.ID)
			if err := w.store.UpdateConnectionStatus(ctx, conn.ID, StatusTokenRevoked); err != nil {
				w.log.Error("tinvest: recording a revoked token failed", "connection_id", conn.ID, "err", err)
			}
			papers = papers[n:]
			continue
		}
		w.log.Info("tinvest: papers classified", "papers", n)
		return err
	}
	n, err := w.classify(ctx, nil, papers)
	w.log.Info("tinvest: papers classified without the broker", "papers", n)
	return err
}

// classify asks and records each paper's class in turn, returning how many
// are done. A paper nobody knows is recorded without a country or sector, so
// it is asked again only when its class is due.
func (w *classesWorker) classify(ctx context.Context, client *Client, papers []instrument.Instrument) (int, error) {
	for i, p := range papers {
		c, err := w.classOf(ctx, client, p)
		if err != nil {
			return i, err
		}
		c.FetchedAt = w.now()
		if err := w.classes.SetClass(ctx, p.ID, c); err != nil {
			return i, err
		}
	}
	return len(papers), nil
}

// classOf finds the paper at the broker by its ISIN and reads its passport;
// a share the broker does not know, or any without a client, asks sectors.
func (w *classesWorker) classOf(ctx context.Context, client *Client, p instrument.Instrument) (instrument.Class, error) {
	c := instrument.Class{Source: instrument.ClassFromTInvest}
	if client != nil {
		found, err := client.FindInstruments(ctx, p.ISIN)
		if err != nil {
			return c, err
		}
		for _, l := range found {
			if !strings.EqualFold(l.ISIN, p.ISIN) || brokerInstrumentKinds[l.Kind] != p.Type {
				continue
			}
			passport, err := client.PassportByUID(ctx, l.UID, l.Kind)
			if err != nil {
				return c, err
			}
			if isoCountry.MatchString(passport.Country) {
				c.Country = passport.Country
			}
			c.Sector = instrument.SectorFromTInvest(p.Type, p.Name, passport.Sector, passport.Focus)
			c.Raw = strings.Trim(fmt.Sprintf("%s %s", passport.Sector, passport.Focus), " ")
			return c, nil
		}
	}
	if w.sectors == nil || (client != nil && p.Type != instrument.TypeShare) {
		return c, nil
	}
	c.Source = instrument.ClassFromYahoo
	if p.Type != instrument.TypeShare {
		return c, nil
	}
	sector, ok, err := w.sectors.SectorFor(ctx, p.ISIN)
	if err != nil {
		// A failed search is not a failed run: the paper is asked again when due.
		w.log.Debug("tinvest: Yahoo's sector search failed", "isin", p.ISIN, "err", err)
		return c, nil
	}
	if ok {
		c.Sector, c.Raw = instrument.SectorFromYahoo(sector), sector
	}
	return c, nil
}

var isoCountry = regexp.MustCompile(`^[A-Z]{2}$`)
