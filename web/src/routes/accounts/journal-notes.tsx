import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import type { AccountWithBalance } from "@/api/accounts";

type Journal = NonNullable<AccountWithBalance["journal"]>;

// What a brokerage account's journal says beside the figure the total counts:
// how it stands against the balance, and what it could not count. Every
// figure is the server's; this only words them (the owner's ruling on Р-2,
// 2026-10-02 — an account whose journal disagrees with its balance says so and
// offers to be counted by the balance until its history is complete).
export function JournalNotes({
  account,
  journal,
  onValueBy,
  pending,
}: {
  account: AccountWithBalance;
  journal: Journal;
  // Absent for viewers, who cannot change how an account is counted.
  onValueBy?: (account: AccountWithBalance, byBalance: boolean) => void;
  pending?: boolean;
}) {
  const { t } = useTranslation();
  const rec = journal.reconciliation;
  const byJournal = account.counted_by === "journal";
  const balance = rec ? formatMinor(rec.balance_in_base_minor, journal.currency) : "";
  const difference = rec ? formatMinor(rec.difference_minor, journal.currency) : "";
  const date = rec ? formatDate(rec.balance_as_of) || rec.balance_as_of : "";

  return (
    // Sentences, not figures: they wrap inside a column of their own width
    // rather than stretching the table past the screen.
    <div className="ml-auto grid max-w-72 justify-items-end gap-0.5 text-right text-xs whitespace-normal">
      {byJournal ? (
        <div className="text-muted-foreground" title={t("accounts.journal.byJournalHint")}>
          {t("accounts.journal.byJournal")}
        </div>
      ) : (
        <div data-testid={`account-journal-pinned-${account.id}`} className="text-muted-foreground">
          {t("accounts.journal.pinned", {
            amount: formatMinor(journal.amount_minor, journal.currency),
          })}
        </div>
      )}
      {rec?.status === "agrees" && (
        <div data-testid={`account-reconciliation-${account.id}`} className="text-emerald-600">
          {t("accounts.journal.agrees", { balance, difference })}
        </div>
      )}
      {rec?.status === "close" && (
        <div
          data-testid={`account-reconciliation-${account.id}`}
          className="text-muted-foreground"
          title={t("accounts.journal.closeHint")}
        >
          {t("accounts.journal.close", { balance, difference })}
        </div>
      )}
      {rec?.status === "differs" && (
        <div
          data-testid={`account-reconciliation-${account.id}`}
          className="text-red-600"
          title={t("accounts.journal.differsHint")}
        >
          {t("accounts.journal.differs", { balance, date, difference })}
        </div>
      )}
      {rec?.status === "stale" && (
        <div
          data-testid={`account-reconciliation-${account.id}`}
          className="text-muted-foreground"
          title={t("accounts.journal.staleHint")}
        >
          {t("accounts.journal.stale", { balance, date })}
        </div>
      )}
      {journal.unpriced_positions > 0 && (
        <div className="text-amber-600">
          {t("accounts.journal.unpriced", { count: journal.unpriced_positions })}
        </div>
      )}
      {journal.negative_cash.length > 0 && (
        <div className="text-amber-600" title={t("accounts.journal.negativeCashHint")}>
          {t("accounts.journal.negativeCash", { currencies: journal.negative_cash.join(", ") })}
        </div>
      )}
      {journal.missing_rates.length > 0 && (
        <div className="text-amber-600">
          {t("accounts.journal.missingRates", { currencies: journal.missing_rates.join(", ") })}
        </div>
      )}
      {/* Offered where it helps: an account whose journal disagrees with its
          balance, and any account already pinned, which can always go back. */}
      {onValueBy && (!byJournal || rec?.status === "differs") && (
        <Button
          variant="link"
          size="sm"
          className="h-auto p-0 text-xs"
          disabled={pending}
          onClick={() => onValueBy(account, byJournal)}
        >
          {byJournal ? t("accounts.journal.useBalance") : t("accounts.journal.useJournal")}
        </Button>
      )}
    </div>
  );
}
