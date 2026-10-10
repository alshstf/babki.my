import { useState } from "react";
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
  minorToInput,
  parseToMinor,
} from "@/lib/money";
import { localToday } from "@/lib/dates";
import {
  useSaveOperation,
  isConflict,
  type Operation,
  type OperationType,
} from "@/api/operations";
import type { AccountWithBalance } from "@/api/accounts";
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
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account: AccountWithBalance;
  // The recorded operation this dialog was opened on, to be corrected in place.
  editing?: Operation;
  preset?: CashPreset;
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

  useOnOpen(open, () => {
    setType(editing?.type ?? (preset ? PRESET_TYPE[preset] : "deposit"));
    setAmount(editing ? minorToInput(editing.amount_minor) : "");
    setOccurredOn(editing?.occurred_on ?? localToday());
    setNote(editing?.note ?? "");
    setCategoryId(editing?.category_id ?? null);
    setCounterparty(editing?.counterparty ?? "");
    setChosen(editing?.category_id != null);
    setMemberId(editing?.member_id ?? null);
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

  const submit = () => {
    if (!amountValid || parsed === null) return;
    createOperation.mutate(
      {
        account_id: account.id,
        type,
        occurred_on: occurredOn,
        amount_minor: isCredit ? parsed : -parsed,
        currency: account.currency,
        note,
        category_id: categoryId,
        counterparty: counterparty.trim(),
        member_id: memberId,
      },
      { onSuccess: () => onOpenChange(false) },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm" onKeyDown={submitOnEnter(submit, valid && !createOperation.isPending)}>
        <DialogHeader>
          <DialogTitle>
            {editing ? t("operations.editTitle") : preset ? t(`cash.presetTitle.${preset}`) : t("cash.title")}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
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
          <AmountField
            id="cash-amount"
            label={t("cash.amount", { currency: account.currency })}
            value={amount}
            onChange={setAmount}
            currency={account.currency}
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
                shared={account.owner_user_id == null}
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
