import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useAccounts, type AccountWithBalance } from "@/api/accounts";
import { useLoan, useRecordLoanPayment, useSetLoanTerms, type Loan, type LoanTerms } from "@/api/loans";
import { useSaveOperation } from "@/api/operations";
import { formatMinor, minorToInput, parseToMinor } from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";

// LoanPanel is a loan account's terms and schedule: the next payment, the debt
// the schedule leaves, the interest it charges in all, and a way to record a
// payment — the interest as a spending, the rest a transfer to the loan.
export function LoanPanel({ account, canEdit }: { account: AccountWithBalance; canEdit: boolean }) {
  const { t } = useTranslation();
  const loan = useLoan(account.id, true);
  const [editing, setEditing] = useState(false);
  const [paying, setPaying] = useState(false);
  const [showAll, setShowAll] = useState(false);
  const issue = useSaveOperation();
  const data = loan.data;
  const c = account.currency;
  // A loan whose journal is not below zero has no debt in it: the money lent
  // was never written out. One withdrawal on the day it was lent puts it there,
  // an ordinary row like the opening balance of a card.
  const debtMissing = !account.journal || account.journal.amount_minor >= 0;

  if (loan.isPending) return null;
  if (!data) {
    return (
      <div className="flex flex-wrap items-center gap-3 rounded-lg border border-dashed p-4 text-sm" data-testid="loan-panel">
        <span className="text-muted-foreground">{t("loan.noTerms")}</span>
        {canEdit && (
          <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
            {t("loan.setTerms")}
          </Button>
        )}
        {editing && <TermsDialog account={account} onClose={() => setEditing(false)} />}
      </div>
    );
  }
  const today = localToday();
  const rows = showAll ? data.schedule : data.schedule.filter((r) => r.on >= today).slice(0, 6);
  return (
    <div className="grid gap-3 rounded-lg border p-4" data-testid="loan-panel">
      <div className="grid gap-3 sm:grid-cols-3">
        <div>
          <div className="text-sm text-muted-foreground">{t("loan.next")}</div>
          <div className="text-xl font-semibold tabular-nums" data-testid="loan-next">
            {data.next ? formatMinor(data.next.payment_minor, c) : "—"}
          </div>
          {data.next && <div className="text-xs text-muted-foreground">{formatDate(data.next.on)}</div>}
        </div>
        <div>
          <div className="text-sm text-muted-foreground">{t("loan.leftBySchedule")}</div>
          <div className="text-xl font-semibold tabular-nums" data-testid="loan-left">
            {formatMinor(data.left_by_schedule_minor, c)}
          </div>
        </div>
        <div>
          <div className="text-sm text-muted-foreground">{t("loan.totalInterest")}</div>
          <div className="text-xl font-semibold tabular-nums">{formatMinor(data.total_interest_minor, c)}</div>
        </div>
      </div>
      <div className="text-xs text-muted-foreground">
        {t("loan.termsLine", {
          amount: formatMinor(data.terms.principal_minor, c),
          rate: Number(data.terms.annual_rate).toLocaleString("ru-RU"),
          months: data.terms.term_months,
          date: formatDate(data.terms.issued_on),
          kind: data.terms.kind === "annuity" ? t("loan.annuity") : t("loan.differentiated"),
        })}
      </div>
      {canEdit && debtMissing && (
        <div className="text-sm text-amber-700 dark:text-amber-400" data-testid="loan-no-debt">
          {t("loan.noDebt")}{" "}
          <button
            type="button"
            className="underline underline-offset-2"
            disabled={issue.isPending}
            onClick={() =>
              issue.mutate({
                account_id: account.id,
                type: "withdrawal",
                occurred_on: data.terms.issued_on,
                amount_minor: -data.terms.principal_minor,
                currency: c,
                note: t("loan.issueNote"),
              })
            }
          >
            {t("loan.recordIssue", {
              amount: formatMinor(data.terms.principal_minor, c),
              date: formatDate(data.terms.issued_on),
            })}
          </button>
        </div>
      )}
      {canEdit && (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={() => setPaying(true)} disabled={!data.next}>
            {t("loan.pay")}
          </Button>
          <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
            {t("loan.editTerms")}
          </Button>
        </div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("loan.columns.date")}</TableHead>
              <TableHead className="text-right">{t("loan.columns.payment")}</TableHead>
              <TableHead className="hidden text-right sm:table-cell">{t("loan.columns.interest")}</TableHead>
              <TableHead className="hidden text-right sm:table-cell">{t("loan.columns.principal")}</TableHead>
              <TableHead className="text-right">{t("loan.columns.left")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.on} data-testid="loan-row">
                <TableCell>{formatDate(r.on)}</TableCell>
                <TableCell className="text-right tabular-nums">{formatMinor(r.payment_minor, c)}</TableCell>
                <TableCell className="hidden text-right tabular-nums sm:table-cell">{formatMinor(r.interest_minor, c)}</TableCell>
                <TableCell className="hidden text-right tabular-nums sm:table-cell">{formatMinor(r.principal_minor, c)}</TableCell>
                <TableCell className="text-right tabular-nums">{formatMinor(r.left_minor, c)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <Button variant="link" size="sm" className="justify-self-start p-0" onClick={() => setShowAll(!showAll)}>
        {showAll ? t("loan.showNext") : t("loan.showAll", { count: data.schedule.length })}
      </Button>
      <p className="text-xs text-muted-foreground">{t("loan.hint")}</p>
      {editing && <TermsDialog account={account} loan={data} onClose={() => setEditing(false)} />}
      {paying && data.next && <PaymentDialog account={account} loan={data} onClose={() => setPaying(false)} />}
    </div>
  );
}

function TermsDialog({ account, loan, onClose }: { account: AccountWithBalance; loan?: Loan; onClose: () => void }) {
  const { t } = useTranslation();
  const save = useSetLoanTerms(account.id);
  const [amount, setAmount] = useState(loan ? minorToInput(loan.terms.principal_minor) : "");
  const [rate, setRate] = useState(loan?.terms.annual_rate ?? "");
  const [months, setMonths] = useState(loan ? String(loan.terms.term_months) : "");
  const [issued, setIssued] = useState(loan?.terms.issued_on ?? localToday());
  const [kind, setKind] = useState<LoanTerms["kind"]>(loan?.terms.kind ?? "annuity");
  const principal = parseToMinor(amount);
  const term = Number(months);
  const rateNum = Number(rate.replace(",", "."));
  const valid = principal !== null && principal > 0 && Number.isInteger(term) && term >= 1 && term <= 600 && rate !== "" && rateNum >= 0 && issued !== "";

  const submit = () => {
    if (!valid || principal === null) return;
    save.mutate(
      { principal_minor: principal, annual_rate: String(rateNum), term_months: term, issued_on: issued, kind },
      { onSuccess: onClose },
    );
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-sm" data-testid="loan-terms-dialog">
        <DialogHeader>
          <DialogTitle>{t("loan.termsTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1">
            <Label htmlFor="loan-amount">{t("loan.amount", { currency: account.currency })}</Label>
            <Input id="loan-amount" inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="loan-rate">{t("loan.rate")}</Label>
            <Input id="loan-rate" inputMode="decimal" value={rate} onChange={(e) => setRate(e.target.value)} />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="loan-months">{t("loan.months")}</Label>
            <Input id="loan-months" inputMode="numeric" value={months} onChange={(e) => setMonths(e.target.value)} />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="loan-issued">{t("loan.issued")}</Label>
            <Input id="loan-issued" type="date" value={issued} onChange={(e) => setIssued(e.target.value)} />
          </div>
          <div className="grid gap-1">
            <Label>{t("loan.kind")}</Label>
            <Select value={kind} onValueChange={(v) => setKind(v as LoanTerms["kind"])}>
              <SelectTrigger aria-label={t("loan.kind")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="annuity">{t("loan.annuity")}</SelectItem>
                <SelectItem value="differentiated">{t("loan.differentiated")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("loan.failed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

const PAYER_ORDER: AccountWithBalance["type"][] = ["checking", "credit_card", "savings", "cash", "deposit"];

function PaymentDialog({ account, loan, onClose }: { account: AccountWithBalance; loan: Loan; onClose: () => void }) {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const record = useRecordLoanPayment(account.id);
  const next = loan.next;
  // A current account first: that is where a loan is usually paid from.
  const payers = (accounts.data ?? [])
    .filter((a) => a.id !== account.id && a.currency === account.currency && a.status === "active" && PAYER_ORDER.includes(a.type))
    .sort((a, b) => PAYER_ORDER.indexOf(a.type) - PAYER_ORDER.indexOf(b.type));
  // The first fitting account until another is picked: the list may arrive
  // after the dialog opens.
  const [picked, setFrom] = useState("");
  const from = picked || (payers[0]?.id ?? "");
  const [on, setOn] = useState(next && next.on <= localToday() ? next.on : localToday());
  const [principal, setPrincipal] = useState(next ? minorToInput(next.principal_minor) : "");
  const [interest, setInterest] = useState(next ? minorToInput(next.interest_minor) : "");
  const p = parseToMinor(principal || "0");
  const i = parseToMinor(interest || "0");
  const valid = from !== "" && on !== "" && p !== null && i !== null && p >= 0 && i >= 0 && p + i > 0;

  const submit = () => {
    if (!valid || p === null || i === null) return;
    record.mutate({ from_account_id: from, occurred_on: on, principal_minor: p, interest_minor: i }, { onSuccess: onClose });
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-sm" data-testid="loan-payment-dialog">
        <DialogHeader>
          <DialogTitle>{t("loan.payTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1">
            <Label>{t("loan.from")}</Label>
            <Select value={from} onValueChange={setFrom}>
              <SelectTrigger aria-label={t("loan.from")}>
                <SelectValue placeholder={t("loan.noPayers")} />
              </SelectTrigger>
              <SelectContent>
                {payers.map((a) => (
                  <SelectItem key={a.id} value={a.id}>
                    {a.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="loan-pay-date">{t("loan.payDate")}</Label>
            <Input id="loan-pay-date" type="date" value={on} onChange={(e) => setOn(e.target.value)} />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="loan-pay-principal">{t("loan.payPrincipal")}</Label>
            <Input id="loan-pay-principal" inputMode="decimal" value={principal} onChange={(e) => setPrincipal(e.target.value)} />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="loan-pay-interest">{t("loan.payInterest")}</Label>
            <Input id="loan-pay-interest" inputMode="decimal" value={interest} onChange={(e) => setInterest(e.target.value)} />
          </div>
          <p className="text-xs text-muted-foreground">{t("loan.payHint")}</p>
          {record.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("loan.payFailed")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || record.isPending} onClick={submit}>
            {t("loan.payButton")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
