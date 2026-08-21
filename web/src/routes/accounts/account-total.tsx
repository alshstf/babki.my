import { useTranslation } from "react-i18next";
import { formatMinor, signClass } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import type { AccountTotal as AccountTotalPayload } from "@/api/positions";

// The account's headline figure: WHAT IT HAS MADE, ALL IN — the closed deals,
// the payments the papers made, the revaluation of what is still held, and the
// account's own charges beside them (interest, standalone commissions, the tax
// taken from the account rather than from a payment).
//
// It replaced the free-cash balance that used to be the biggest number here.
// Cash is a fact about the account, not an answer to «сколько я тут заработал»,
// and it now sits above the positions where the rest of the holdings are.
//
// DEPOSITS AND WITHDRAWALS ARE NOT IN IT: putting money in is not earning it.
// Neither is the revaluation of idle cash — no position covers it and this
// figure does not claim it, which the tooltip says rather than leaving a reader
// to find out by adding things up.
//
// TWO MODES, TWO SHAPES. In the base currency there is one number, which is the
// only form in which an account holding rubles, dollars and yuan has a single
// answer. In the positions' own currencies there are several — one per currency
// — because adding them would produce an integer denominated in nothing.
export function AccountTotal({
  total,
  mode,
}: {
  total: AccountTotalPayload;
  mode: DisplayCurrencyMode;
}) {
  const { t } = useTranslation();

  // In base mode the server publishes either the figure or the reason there is
  // none. In native mode every bucket is published, and a bucket can still be
  // null — a position that realized into another currency has no own-currency
  // total to add — which is shown as its own line rather than dropped, or the
  // account would silently have one fewer currency than it holds.
  const figures =
    mode === "base"
      ? total.in_base == null
        ? []
        : [{ currency: total.base_currency, amountMinor: total.in_base }]
      : total.by_currency
          .filter((entry) => entry.amount_minor != null)
          .map((entry) => ({
            currency: entry.currency,
            amountMinor: entry.amount_minor as number,
          }));
  const unknowable =
    mode === "base"
      ? []
      : total.by_currency.filter((entry) => entry.amount_minor == null);

  // The two assumptions the figure rests on, each published as a count by the
  // server. They are not gaps — the number IS there — so they are said beside
  // it rather than in place of it, and they pull it in opposite directions:
  // a paper written off makes the total too low, a paper whose price nobody
  // recorded makes it too high.
  const zeroValued = total.zero_valued_positions;
  const unknownCost = total.unknown_cost_positions;
  const zeroValuedCost = total.zero_valued_cost_by_currency
    .map((entry) => formatMinor(entry.amount_minor, entry.currency))
    .join(" · ");

  if (
    figures.length === 0 &&
    unknowable.length === 0 &&
    total.in_base_gap == null
  )
    return null;

  return (
    <div className="mt-2 grid gap-0.5" data-testid="account-total">
      <div className="flex flex-wrap items-baseline gap-x-3">
        {figures.map((figure) => (
          <span
            key={figure.currency}
            data-testid="account-total-amount"
            className={cn(
              "text-2xl font-bold tabular-nums",
              signClass(figure.amountMinor),
            )}
          >
            {formatMinor(figure.amountMinor, figure.currency)}
          </span>
        ))}
        {mode === "base" && total.in_base_gap != null && (
          <span
            data-testid="account-total-gap"
            className="text-sm text-muted-foreground"
            title={
              total.no_rate_currencies.length > 0
                ? t("positions.accountTotalGapCurrenciesHint")
                : undefined
            }
          >
            {/* WHICH MONEY, when the server could name it. «Нет курса» alone
                sends a reader to wait for a backfill, and a source that does not
                quote a currency at all has nothing to fetch — the Bank of Russia
                publishes none for XAU, the code the broker uses for gold. */}
            {total.no_rate_currencies.length > 0
              ? t("positions.accountTotalGapCurrencies", {
                  currencies: total.no_rate_currencies.join(", "),
                })
              : t("positions.accountTotalGap")}
          </span>
        )}
      </div>
      <div
        data-testid="account-total-label"
        className="text-xs text-muted-foreground"
        title={t("positions.accountTotalHint")}
      >
        {t("positions.accountTotalTitle")}
      </div>
      {unknowable.length > 0 && (
        <div
          data-testid="account-total-unknowable"
          className="text-xs text-muted-foreground"
        >
          {t("positions.accountTotalUnknowable", {
            currencies: unknowable.map((entry) => entry.currency).join(", "),
          })}
        </div>
      )}
      {zeroValued > 0 && (
        <div
          data-testid="account-total-zero-valued"
          className="text-xs text-muted-foreground"
          title={t("positions.accountTotalZeroValuedHint")}
        >
          {t("positions.accountTotalZeroValued", {
            count: zeroValued,
            cost: zeroValuedCost,
          })}
        </div>
      )}
      {unknownCost > 0 && (
        <div
          data-testid="account-total-unknown-cost"
          className="text-xs text-muted-foreground"
          title={t("positions.accountTotalUnknownCostHint")}
        >
          {t("positions.accountTotalUnknownCost", { count: unknownCost })}
        </div>
      )}
    </div>
  );
}
