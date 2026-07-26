package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
)

func newSeedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "seed",
		Short: "Наполнить пустой инстанс демо-данными (демо-семья и счета)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx()
			defer stop()
			r, err := setup(ctx, true)
			if err != nil {
				return err
			}
			defer r.close()
			if err := seedDemo(ctx, r.pool); err != nil {
				return err
			}
			r.log.Info("demo data seeded", "login", "demo", "password", "demo1234")
			return nil
		},
	}
}

// seedDemo populates an empty instance with a demo family and accounts.
func seedDemo(ctx context.Context, pool *pgxpool.Pool) error {
	famStore := family.NewStore(pool)
	svc := family.NewService(famStore)

	needed, err := svc.SetupNeeded(ctx)
	if err != nil {
		return err
	}
	if !needed {
		return fmt.Errorf("instance already has users; seed works only on an empty instance")
	}

	_, owner, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "Демо-семья", Username: "demo", DisplayName: "Александр", Password: "demo1234",
	})
	if err != nil {
		return err
	}
	if _, err := svc.CreateMember(ctx, owner, "partner", "Партнёр", "demo1234", family.RoleEditor); err != nil {
		return err
	}

	accStore := account.NewStore(pool)
	d := func(s string) time.Time {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			panic(err)
		}
		return t
	}
	dates := []time.Time{d("2026-05-31"), d("2026-06-30"), d("2026-07-20")}

	type seedAcc struct {
		name        string
		typ         account.Type
		currency    string
		institution string
		personal    bool
		balances    [3]int64 // minor units per date
	}
	seeds := []seedAcc{
		{
			"Брокерский Т-Банк", account.TypeBrokerage, "RUB", "Т-Банк", false,
			[3]int64{1_250_000_00, 1_310_000_00, 1_385_000_00},
		},
		{
			"Freedom KZ", account.TypeBrokerage, "USD", "Freedom Finance", false,
			[3]int64{8_200_00, 8_350_00, 8_500_00},
		},
		{
			"Текущий Сбер", account.TypeChecking, "RUB", "Сбер", false,
			[3]int64{145_000_00, 210_000_00, 180_000_00},
		},
		{
			"Вклад ГПБ", account.TypeDeposit, "RUB", "Газпромбанк", false,
			[3]int64{500_000_00, 500_000_00, 500_000_00},
		},
		{
			"Кредитка Альфа", account.TypeCreditCard, "RUB", "Альфа-Банк", false,
			[3]int64{-92_000_00, -45_000_00, -61_500_00},
		},
		{
			"Наличные", account.TypeCash, "RUB", "", true,
			[3]int64{70_000_00, 70_000_00, 70_000_00},
		},
	}
	for _, s := range seeds {
		var personalOwner *uuid.UUID
		if s.personal {
			personalOwner = &owner.UserID
		}
		a, err := accStore.Create(ctx, owner.SpaceID, personalOwner, s.name, s.typ, s.currency, s.institution)
		if err != nil {
			return fmt.Errorf("seed account %q: %w", s.name, err)
		}
		for i, date := range dates {
			if err := accStore.SetBalance(ctx, owner.SpaceID, a.ID, date, s.balances[i]); err != nil {
				return fmt.Errorf("seed balance %q %s: %w", s.name, date.Format("2006-01-02"), err)
			}
		}
	}
	return nil
}
