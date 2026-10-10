import { useRef, useState } from "react";
import { ScanLine } from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { AmountField, OperationDateField } from "@/components/form-fields";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  formatMinor,
  minorToInput,
  parseToMinor,
} from "@/lib/money";
import { formatDate, localToday } from "@/lib/dates";
import { parseReceiptQr, receiptIsIncoming, type Receipt } from "@/lib/receipt-qr";
import { decodeQrFromImage } from "@/lib/qr-decode";
import {
  useSaveOperation,
  isConflict,
  type Operation,
  type OperationType,
} from "@/api/operations";
import { attachReceipt, createReceipt, matchReceipt, type ReceiptMatch, type ReceiptNew } from "@/api/receipts";
import { useAccounts, type AccountWithBalance } from "@/api/accounts";
import { matchRule, useCategories, useCategoryRules } from "@/api/categories";
import { useMembers } from "@/api/members";
import { MemberSelect } from "@/components/member-picker";
import { CategorySelect, categoryKindOf } from "@/components/category-picker";
import { MAX_NOTE } from "@/lib/text-limits";
import { submitOnEnter } from "@/lib/submit-on-enter";
import { useOnOpen } from "@/lib/use-on-open";

// Cash-level journal entries: no instrument attribution, only a signed cash
// effect on the account. The backend enforces the sign strictly per type
// (see internal/operation/service.go validate()) — CREDIT_TYPES must be
// positive, everything else here must be negative — so the user only ever
// types a positive number and this dialog applies the correct sign.
const CASH_TYPES: OperationType[] = ["deposit", "withdrawal", "fee", "tax", "interest"];
const CREDIT_TYPES = new Set<OperationType>(["deposit", "interest"]);
const MAX_COUNTERPARTY = 200;

// A spending and an earning are a withdrawal and a deposit with a category;
// the dialog opened as one of them starts there and says so in its title.
export type CashPreset = "expense" | "income";
const PRESET_TYPE: Record<CashPreset, OperationType> = { expense: "withdrawal", income: "deposit" };

export function CashDialog({
  open,
  onOpenChange,
  account,
  editing,
  preset,
  accounts,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  // The recorded operation this dialog was opened on, to be corrected in place.
  editing?: Operation;
  preset?: CashPreset;
  // The accounts the row may go to instead, for the quick add: the dialog
  // opens on account and offers these in a list above the rest.
  accounts?: AccountWithBalance[];
  onSaved?: (op: Operation) => void;
}) {
  const { t } = useTranslation();
  const createOperation = useSaveOperation(editing?.id);
  const categories = useCategories();
  const rules = useCategoryRules();
  const members = useMembers();
  // Whose the row is matters where more than one person spends.
  const family = members.data ?? [];

  const [type, setType] = useState<OperationType>("deposit");
  const [amount, setAmount] = useState("");
  const [occurredOn, setOccurredOn] = useState(localToday());
  const [note, setNote] = useState("");
  const [categoryId, setCategoryId] = useState<string | null>(null);
  const [counterparty, setCounterparty] = useState("");
  // Whether the category is the person's choice; until then the family's
  // rules suggest one from the counterparty.
  const [chosen, setChosen] = useState(false);
  const [memberId, setMemberId] = useState<string | null>(null);
  const [pickedId, setPickedId] = useState(account.id);
  // Reading a receipt's QR code: what happened last, for the line under the
  // button.
  const [receipt, setReceipt] = useState<"reading" | "filled" | "noCode" | "notReceipt" | null>(null);
  // The receipt read, to be recorded with the row; the row it was written to
  // before (or "waiting" with none); the rows of its total near its day it
  // may complete instead — the bank's row of the same card purchase.
  const [scanned, setScanned] = useState<Receipt | null>(null);
  const [already, setAlready] = useState<ReceiptMatch | "waiting" | null>(null);
  const [candidates, setCandidates] = useState<ReceiptMatch[]>([]);
  // A receipt loaded before (a statement, a letter) and waiting for its row:
  // the row saved or picked here completes it.
  const [waitingId, setWaitingId] = useState<string | null>(null);
  const [attachFailed, setAttachFailed] = useState(false);
  const allAccounts = useAccounts();
  const photo = useRef<HTMLInputElement>(null);
  const target = accounts?.find((a) => a.id === pickedId) ?? account;

  useOnOpen(open, () => {
    setType(editing?.type ?? (preset ? PRESET_TYPE[preset] : "deposit"));
    setAmount(editing ? minorToInput(editing.amount_minor) : "");
    setOccurredOn(editing?.occurred_on ?? localToday());
    setNote(editing?.note ?? "");
    setCategoryId(editing?.category_id ?? null);
    setCounterparty(editing?.counterparty ?? "");
    setChosen(editing?.category_id != null);
    setMemberId(editing?.member_id ?? null);
    setPickedId(account.id);
    setReceipt(null);
    setScanned(null);
    setAlready(null);
    setCandidates([]);
    setWaitingId(null);
    setAttachFailed(false);
    createOperation.reset();
  });

  const typeCounterparty = (next: string) => {
    setCounterparty(next);
    if (chosen) return;
    setCategoryId(
      matchRule(rules.data ?? [], categories.data ?? [], categoryKindOf(type), { counterparty: next, note }) ?? null,
    );
  };

  // A category of the other direction does not survive a change of type.
  const changeType = (next: OperationType) => {
    if (categoryKindOf(next) !== categoryKindOf(type)) setCategoryId(null);
    setType(next);
  };

  const isCredit = CREDIT_TYPES.has(type);
  const parsed = parseToMinor(amount);
  const amountValid = parsed !== null && parsed > 0;
  const valid = amountValid && occurredOn !== "";

  // A photo of a receipt fills the total, the day and which way the money
  // went; the shop is not in the code, the person names it. The fiscal
  // numbers go to the note and the receipt itself is recorded with the row.
  const readReceipt = async (file: File | undefined) => {
    if (!file) return;
    setReceipt("reading");
    setScanned(null);
    setAlready(null);
    setCandidates([]);
    setWaitingId(null);
    let text: string | null = null;
    try {
      text = await decodeQrFromImage(file);
    } catch {
      text = null;
    }
    if (!text) {
      setReceipt("noCode");
      return;
    }
    const r = parseReceiptQr(text);
    if (!r) {
      setReceipt("notReceipt");
      return;
    }
    changeType(receiptIsIncoming(r.kind) ? "deposit" : "withdrawal");
    setAmount(minorToInput(r.amountMinor));
    setOccurredOn(r.date);
    if (note.trim() === "") {
      setNote(t("cash.receipt.note", { date: formatDate(r.date), time: r.time, fn: r.fn, fd: r.fd }));
    }
    setReceipt("filled");
    setScanned(r);
    // Only advice: a lookup that fails leaves the form as filled.
    matchReceipt(receiptQuery(r))
      .then((found) => {
        if (found.receipt && found.written_to) {
          setAlready(found.written_to);
          return;
        }
        if (found.receipt) {
          setAlready("waiting");
          setWaitingId(found.receipt.id);
        }
        setCandidates(found.candidates);
      })
      .catch(() => setCandidates([]));
  };

  // The receipt completes the row: the one saved here, or the bank's row it
  // was taken for. One whose row was changed past its total waits for one.
  const record = async (r: Receipt, operationId: string) => {
    if (waitingId) {
      await attachReceipt(waitingId, operationId).catch(() => undefined);
      return;
    }
    const body: ReceiptNew = { ...receiptQuery(r), fp: r.fp, operation_id: operationId, source: "qr" };
    try {
      await createReceipt(body);
    } catch {
      await createReceipt({ ...body, operation_id: null }).catch(() => undefined);
    }
  };

  const attach = (to: ReceiptMatch) => {
    if (!scanned) return;
    setAttachFailed(false);
    (waitingId ? attachReceipt(waitingId, to.id) : createReceipt({ ...receiptQuery(scanned), fp: scanned.fp, operation_id: to.id, source: "qr" }))
      .then(() => onOpenChange(false))
      .catch(() => setAttachFailed(true));
  };

  const submit = () => {
    if (!amountValid || parsed === null) return;
    createOperation.mutate(
      {
        account_id: target.id,
        type,
        occurred_on: occurredOn,
        amount_minor: isCredit ? parsed : -parsed,
        currency: target.currency,
        note,
        category_id: categoryId,
        counterparty: counterparty.trim(),
        member_id: memberId,
      },
      {
        onSuccess: (op) => {
          if (scanned) void record(scanned, op.id);
          onSaved?.(op);
          onOpenChange(false);
        },
      },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="sm:max-w-sm"
        onKeyDown={submitOnEnter(submit, valid && !createOperation.isPending)}
        // The quick add starts at the amount: on a phone the keyboard opens
        // at once, the account and the date being already the likely ones.
        onOpenAutoFocus={(e) => {
          if (!accounts) return;
          e.preventDefault();
          document.getElementById("cash-amount")?.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>
            {editing ? t("operations.editTitle") : preset ? t(`cash.presetTitle.${preset}`) : t("cash.title")}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          {accounts && (
            <div className="grid gap-2">
              <Label htmlFor="cash-account">{t("cash.account")}</Label>
              <Select value={target.id} onValueChange={setPickedId}>
                <SelectTrigger id="cash-account"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {accounts.map((a) => (
                    <SelectItem key={a.id} value={a.id}>
                      {a.name} · {a.currency}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}
          <div className="grid gap-2">
            <Label htmlFor="cash-type">{t("cash.type")}</Label>
            <Select
              value={type}
              onValueChange={(v) => changeType(v as OperationType)}
              disabled={editing !== undefined}
            >
              <SelectTrigger id="cash-type"><SelectValue /></SelectTrigger>
              <SelectContent>
                {CASH_TYPES.map((cashType) => (
                  <SelectItem key={cashType} value={cashType}>
                    {t(`operationTypes.${cashType}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {preset && !editing && (
            <div className="grid gap-1">
              <input
                ref={photo}
                type="file"
                accept="image/*"
                capture="environment"
                className="hidden"
                data-testid="receipt-photo"
                onChange={(e) => {
                  void readReceipt(e.target.files?.[0]);
                  e.target.value = "";
                }}
              />
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="justify-self-start"
                disabled={receipt === "reading"}
                onClick={() => photo.current?.click()}
              >
                <ScanLine className="size-4" />
                {t("cash.receipt.button")}
              </Button>
              {receipt && (
                <p
                  className={receipt === "noCode" || receipt === "notReceipt" ? "text-xs text-amber-700 dark:text-amber-400" : "text-xs text-muted-foreground"}
                  data-testid="receipt-status"
                >
                  {t(`cash.receipt.${receipt}`)}
                </p>
              )}
              {already && (
                <p className={already === "waiting" ? "text-xs text-muted-foreground" : "text-xs text-amber-700 dark:text-amber-400"} data-testid="receipt-already">
                  {already === "waiting"
                    ? t("cash.receipt.alreadyWaiting")
                    : t("cash.receipt.already", {
                        date: formatDate(already.occurred_on),
                        amount: formatMinor(Math.abs(already.amount_minor), already.currency),
                      })}
                </p>
              )}
              {candidates.length > 0 && (
                <div className="grid gap-1 rounded-md border border-amber-300 p-2 dark:border-amber-800" data-testid="receipt-candidates">
                  <p className="text-xs">{t("cash.receipt.candidates")}</p>
                  {candidates.map((c) => (
                    <div key={c.id} className="flex flex-wrap items-center justify-between gap-2 text-xs">
                      <span>
                        {allAccounts.data?.find((a) => a.id === c.account_id)?.name ?? ""} · {formatDate(c.occurred_on)} ·{" "}
                        {formatMinor(Math.abs(c.amount_minor), c.currency)}
                      </span>
                      <Button type="button" size="sm" variant="outline" onClick={() => attach(c)}>
                        {t("cash.receipt.attach")}
                      </Button>
                    </div>
                  ))}
                  {attachFailed && <p className="text-xs text-red-700 dark:text-red-400">{t("cash.receipt.attachFailed")}</p>}
                </div>
              )}
            </div>
          )}
          <AmountField
            id="cash-amount"
            label={t("cash.amount", { currency: target.currency })}
            value={amount}
            onChange={setAmount}
            currency={target.currency}
            accepted={amountValid}
            badNumber={t("cash.badNumber")}
            hint={
              <p className="text-xs text-muted-foreground">
                {isCredit ? t("cash.creditHint") : t("cash.debitHint")}
              </p>
            }
          />
          <OperationDateField id="cash-date" label={t("cash.date")} value={occurredOn} onChange={setOccurredOn} />
          <div className="grid gap-2">
            <Label htmlFor="cash-category">{t("cash.category")}</Label>
            <CategorySelect
              id="cash-category"
              categories={categories.data ?? []}
              kind={categoryKindOf(type)}
              value={categoryId}
              onChange={(v) => {
                setChosen(true);
                setCategoryId(v);
              }}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="cash-counterparty">
              {categoryKindOf(type) === "income" ? t("cash.counterpartyFrom") : t("cash.counterpartyTo")}
            </Label>
            <Input
              id="cash-counterparty"
              maxLength={MAX_COUNTERPARTY}
              value={counterparty}
              onChange={(e) => typeCounterparty(e.target.value)}
            />
          </div>
          {family.length > 1 && (
            <div className="grid gap-2">
              <Label htmlFor="cash-member">{t("cash.member")}</Label>
              <MemberSelect
                id="cash-member"
                members={family}
                value={memberId}
                onChange={setMemberId}
                shared={target.owner_user_id == null}
              />
            </div>
          )}
          <div className="grid gap-2">
            <Label htmlFor="cash-note">{t("cash.note")}</Label>
            <Input id="cash-note" maxLength={MAX_NOTE} value={note} onChange={(e) => setNote(e.target.value)} />
          </div>
          {createOperation.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(createOperation.error) ? t("operations.conflict") : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || createOperation.isPending} onClick={submit}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// receiptQuery is a receipt read off its QR code as the API names it.
function receiptQuery(r: Receipt) {
  const kind: ReceiptNew["kind"] = r.kind === "payoutRefund" ? "payout_refund" : r.kind;
  return { fn: r.fn, fd: r.fd, kind, total_minor: r.amountMinor, issued_at: `${r.date}T${r.time}` };
}
