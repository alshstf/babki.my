import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
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
import { useSession } from "@/api/session";
import {
  useCreateAccount,
  useUpdateAccount,
  type AccountType,
  type AccountWithBalance,
} from "@/api/accounts";

const ACCOUNT_TYPES: AccountType[] = [
  "brokerage",
  "checking",
  "savings",
  "deposit",
  "credit_card",
  "loan",
  "cash",
];
const COMMON_CURRENCIES = ["RUB", "USD", "EUR", "KZT"];

export function AccountDialog({
  open,
  onOpenChange,
  account,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  account?: AccountWithBalance;
}) {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const create = useCreateAccount();
  const update = useUpdateAccount();
  const isEdit = Boolean(account);
  const mutation = isEdit ? update : create;

  const [name, setName] = useState("");
  const [type, setType] = useState<AccountType>("brokerage");
  const [currency, setCurrency] = useState("RUB");
  const [customCurrency, setCustomCurrency] = useState("");
  const [institution, setInstitution] = useState("");
  const [personal, setPersonal] = useState(false);

  useEffect(() => {
    if (open) {
      setName(account?.name ?? "");
      setType(account?.type ?? "brokerage");
      setCurrency(account?.currency && COMMON_CURRENCIES.includes(account.currency) ? account.currency : account ? "custom" : "RUB");
      setCustomCurrency(account && !COMMON_CURRENCIES.includes(account.currency) ? account.currency : "");
      setInstitution(account?.institution ?? "");
      setPersonal(Boolean(account?.owner_user_id));
      create.reset();
      update.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, account]);

  const effectiveCurrency = currency === "custom" ? customCurrency.toUpperCase() : currency;
  const valid = name.trim() !== "" && /^[A-Z]{3}$/.test(effectiveCurrency);

  const submit = () => {
    const ownerUserId = personal ? (session?.user.id ?? null) : null;
    if (isEdit && account) {
      update.mutate(
        {
          id: account.id,
          body: {
            name,
            institution,
            owner_user_id: ownerUserId,
          },
        },
        { onSuccess: () => onOpenChange(false) },
      );
    } else {
      create.mutate(
        {
          name,
          type,
          currency: effectiveCurrency,
          institution,
          owner_user_id: ownerUserId,
        },
        { onSuccess: () => onOpenChange(false) },
      );
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {isEdit ? t("accounts.dialog.editTitle") : t("accounts.dialog.createTitle")}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="acc-name">{t("accounts.dialog.name")}</Label>
            <Input id="acc-name" value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          {!isEdit && (
            <>
              <div className="grid gap-2">
                <Label>{t("accounts.dialog.type")}</Label>
                <Select value={type} onValueChange={(v) => setType(v as AccountType)}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {ACCOUNT_TYPES.map((accountType) => (
                      <SelectItem key={accountType} value={accountType}>
                        {t(`accountTypes.${accountType}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-2">
                <Label>{t("accounts.dialog.currency")}</Label>
                <Select value={currency} onValueChange={setCurrency}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {COMMON_CURRENCIES.map((code) => (
                      <SelectItem key={code} value={code}>{code}</SelectItem>
                    ))}
                    <SelectItem value="custom">{t("accounts.dialog.otherCurrency")}</SelectItem>
                  </SelectContent>
                </Select>
                {currency === "custom" && (
                  <Input
                    placeholder="ISO-код, например GBP"
                    value={customCurrency}
                    maxLength={3}
                    onChange={(e) => setCustomCurrency(e.target.value)}
                  />
                )}
              </div>
            </>
          )}
          <div className="grid gap-2">
            <Label htmlFor="acc-inst">{t("accounts.dialog.institution")}</Label>
            <Input
              id="acc-inst"
              placeholder={t("accounts.dialog.institutionPlaceholder")}
              value={institution}
              onChange={(e) => setInstitution(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label>{t("accounts.dialog.ownership")}</Label>
            <Select
              value={personal ? "personal" : "shared"}
              onValueChange={(v) => setPersonal(v === "personal")}
            >
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="shared">{t("accounts.dialog.sharedOption")}</SelectItem>
                <SelectItem value="personal">
                  {t("accounts.dialog.personalOption", {
                    name: session?.user.display_name ?? "",
                  })}
                </SelectItem>
              </SelectContent>
            </Select>
          </div>
          {mutation.isError && (
            <Alert variant="destructive">
              <AlertDescription>{mutation.error.message}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || mutation.isPending} onClick={submit}>
            {isEdit ? t("common.save") : t("common.create")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
