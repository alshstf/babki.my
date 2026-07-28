import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useSession } from "@/api/session";
import { useUpdateBaseCurrency } from "@/api/space";
import { ApiError } from "@/api/operations";

const COMMON_CURRENCIES = ["RUB", "USD", "EUR", "KZT"];

export function SettingsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const updateBaseCurrency = useUpdateBaseCurrency();
  const isOwner = session?.role === "owner";

  // The layout route only renders once a session is loaded, so `session` is
  // already available here — safe to seed local state from it once.
  const [currency, setCurrency] = useState(() =>
    session && COMMON_CURRENCIES.includes(session.base_currency) ? session.base_currency : "custom",
  );
  const [customCurrency, setCustomCurrency] = useState(() =>
    session && !COMMON_CURRENCIES.includes(session.base_currency) ? session.base_currency : "",
  );

  if (!isOwner) {
    return (
      <Alert>
        <AlertDescription>{t("settings.ownerOnly")}</AlertDescription>
      </Alert>
    );
  }

  const effectiveCurrency = currency === "custom" ? customCurrency.toUpperCase() : currency;
  const validCurrency = /^[A-Z]{3}$/.test(effectiveCurrency);
  const changed = effectiveCurrency !== session?.base_currency;
  const canSave = validCurrency && changed && !updateBaseCurrency.isPending;

  const save = () => {
    updateBaseCurrency.mutate({ base_currency: effectiveCurrency });
  };

  return (
    <div className="grid gap-6">
      <h1 className="text-2xl font-bold">{t("settings.title")}</h1>
      <Card className="max-w-md">
        <CardHeader>
          <CardTitle>{t("settings.baseCurrency")}</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="grid gap-2">
            <Select
              value={currency}
              onValueChange={(v) => {
                setCurrency(v);
                updateBaseCurrency.reset();
              }}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {COMMON_CURRENCIES.map((code) => (
                  <SelectItem key={code} value={code}>
                    {code}
                  </SelectItem>
                ))}
                <SelectItem value="custom">{t("accounts.dialog.otherCurrency")}</SelectItem>
              </SelectContent>
            </Select>
            {currency === "custom" && (
              <Input
                placeholder={t("accounts.dialog.currencyPlaceholder")}
                value={customCurrency}
                maxLength={3}
                onChange={(e) => {
                  setCustomCurrency(e.target.value);
                  updateBaseCurrency.reset();
                }}
              />
            )}
          </div>
          <p className="text-xs text-muted-foreground">{t("settings.baseCurrencyHint")}</p>
          {updateBaseCurrency.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {updateBaseCurrency.error instanceof ApiError &&
                updateBaseCurrency.error.status === 403
                  ? t("settings.forbidden")
                  : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
          <div>
            <Button disabled={!canSave} onClick={save}>
              {t("common.save")}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
