import { useTranslation } from "react-i18next";
import { formatMinor, signClass } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import type { AccountTotal as AccountTotalPayload } from "@/api/positions";

// The account's headline: what it has made, all in (closed deals, payments,
// revaluation of holdings, and the account's own charges). Deposits and
// withdrawals are not in it, nor the revaluation of idle cash, which the tooltip
// says. One number in base currency; one per currency in native mode, since a
// sum across currencies is denominated in nothing.
export function AccountTotal({
  total,
  mode,
}: {
  total: AccountTotalPayload;
  mode: DisplayCurrencyMode;
}) {
  const { t } = useTranslation();

  // In base mode the figure or its gap. In native mode a bucket can be null
  // (realized into another currency) and is shown as its own line rather
  // than dropped.
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

  // Two assumptions, counted by the server and said beside the figure: a
  // written-off paper makes it too low, an unpriced purchase too high.
  const zeroValued = total.zero_valued_positions;
  // Papers left out of the base figure for want of a purchase date, which
  // never closes (AccountTotal.undated_positions).
  const undated = total.undated_positions;
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
            {/* Which currency, when the server could name it: the Bank of Russia
               quotes no XAU (the broker's gold), so there is nothing to wait for. */}
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
      {/* The currency's share of the figure, named (Р-5): a revaluation of
         money, not a trade's result. Base mode only. */}
      {mode === "base" && total.cash_fx_in_base != null && total.cash_fx_in_base !== 0 && (
        <div
          data-testid="account-total-cash-fx"
          className="text-xs text-muted-foreground"
          title={t("positions.accountTotalCashFxHint")}
        >
          {t("positions.accountTotalCashFx", {
            amount: `${total.cash_fx_in_base > 0 ? "+" : ""}${formatMinor(total.cash_fx_in_base, total.base_currency)}`,
          })}
        </div>
      )}
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
      {undated > 0 && (
        <div
          data-testid="account-total-undated"
          className="text-xs text-muted-foreground"
          title={t("positions.accountTotalUndatedHint")}
        >
          {t("positions.accountTotalUndated", { count: undated })}
        </div>
      )}
      {unknownCost > 0 && (
        <div
          data-testid="account-total-unknown-cost"
          className="text-xs text-amber-600"
          title={t("positions.accountTotalUnknownCostHint")}
        >
          {t("positions.accountTotalUnknownCost", { count: unknownCost })}
        </div>
      )}
    </div>
  );
}
