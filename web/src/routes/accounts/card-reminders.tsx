import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useCreditCards, type CreditCardSummary } from "@/api/credit-cards";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { SOON_DAYS, daysUntil } from "@/lib/card-due";

interface Reminder {
  key: string;
  urgent: boolean;
  text: string;
  card: CreditCardSummary;
}

// CardReminders is what is due on the family's credit cards soon, or already
// missed (decision Р-26: reminders inside the program for now): the minimum
// payment and the sum that keeps purchases free of interest, a week ahead.
export function CardReminders() {
  const { t } = useTranslation();
  const cards = useCreditCards();
  const reminders: Reminder[] = [];
  for (const card of cards.data ?? []) {
    const st = card.status;
    const c = card.currency;
    if (st.minimum_missed && st.minimum_minor > 0) {
      reminders.push({ key: `${card.account_id}-min-missed`, urgent: true, card,
        text: t("cardReminder.minimumMissed", { amount: formatMinor(st.minimum_minor, c), date: formatDate(st.minimum_on) }) });
    }
    for (const l of st.lost) {
      reminders.push({ key: `${card.account_id}-lost-${l.from}`, urgent: true, card,
        text: t("cardReminder.lost", { from: formatDate(l.from), to: formatDate(l.to), amount: formatMinor(l.amount_minor, c) }) });
    }
    const next = st.grace[0];
    if (next && daysUntil(next.on) <= SOON_DAYS) {
      reminders.push({ key: `${card.account_id}-grace`, urgent: false, card,
        text: t("cardReminder.grace", { amount: formatMinor(next.amount_minor, c), date: formatDate(next.on) }) });
    } else if (!st.minimum_missed && st.minimum_minor > 0 && daysUntil(st.minimum_on) <= SOON_DAYS) {
      reminders.push({ key: `${card.account_id}-min`, urgent: false, card,
        text: t("cardReminder.minimum", { amount: formatMinor(st.minimum_minor, c), date: formatDate(st.minimum_on) }) });
    }
  }
  if (reminders.length === 0) return null;
  return (
    <div className="grid gap-2" data-testid="card-reminders">
      {reminders.map((r) => (
        <Alert key={r.key} variant={r.urgent ? "destructive" : "default"} className={r.urgent ? undefined : "border-amber-300 text-amber-800 dark:text-amber-300"}>
          <AlertDescription>
            <Link to="/accounts/$accountId" params={{ accountId: r.card.account_id }} className="font-medium underline underline-offset-2">
              {r.card.name}
            </Link>
            {": "}
            {r.text}
          </AlertDescription>
        </Alert>
      ))}
    </div>
  );
}
