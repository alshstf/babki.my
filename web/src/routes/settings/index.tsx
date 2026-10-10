import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { DataSourcesList } from "@/components/data-sources";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useSession } from "@/api/session";
import { useUpdateSpace, type UpdateSpaceBody } from "@/api/space";
import { useTaxResidencies } from "@/api/tax-residencies";
import {
  useConnections,
  type TinvestConnectionStatus,
} from "@/api/connections";
import { ApiError } from "@/api/operations";
import { CostBasisNotice } from "@/components/cost-basis-notice";
import { countryName } from "@/lib/country";
import { COMMON_CURRENCIES, isCurrencyCode } from "@/lib/currencies";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { queryState, refreshFailed } from "@/lib/query-state";

type FullValuation = NonNullable<UpdateSpaceBody["full_valuation"]>;

const FULL_VALUATIONS: FullValuation[] = ["nav_and_foreign", "nav", "liquid"];

export function SettingsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const updateSpace = useUpdateSpace();
  const isOwner = session?.role === "owner";
  // Only the owner may change either setting, and the list is only ever used
  // by the form below — nobody else's screen pays for the request.
  const residencies = useTaxResidencies(isOwner);

  // The layout route only renders once a session is loaded, so `session` is
  // already available here — safe to seed local state from it once.
  const [currency, setCurrency] = useState(() =>
    session && COMMON_CURRENCIES.includes(session.base_currency)
      ? session.base_currency
      : "custom",
  );
  const [customCurrency, setCustomCurrency] = useState(() =>
    session && !COMMON_CURRENCIES.includes(session.base_currency)
      ? session.base_currency
      : "",
  );
  const [country, setCountry] = useState(() => session?.tax_residency ?? "");
  const [fullValuation, setFullValuation] = useState<FullValuation>(
    () => session?.full_valuation ?? "nav_and_foreign",
  );

  if (!isOwner) {
    return (
      <Alert>
        <AlertDescription>{t("settings.ownerOnly")}</AlertDescription>
      </Alert>
    );
  }

  const effectiveCurrency =
    currency === "custom" ? customCurrency.toUpperCase() : currency;
  const validCurrency = isCurrencyCode(effectiveCurrency);
  const currencyChanged = effectiveCurrency !== session?.base_currency;
  const countryChanged = country !== session?.tax_residency;
  const fullValuationChanged = fullValuation !== session?.full_valuation;
  const canSave =
    (currencyChanged || countryChanged || fullValuationChanged) &&
    (!currencyChanged || validCurrency) &&
    !updateSpace.isPending;

  // Only changed fields are sent; the PATCH is partial, so picking a country
  // never rewrites the base currency.
  const save = () => {
    const body: UpdateSpaceBody = {};
    if (currencyChanged) body.base_currency = effectiveCurrency;
    if (countryChanged) body.tax_residency = country;
    if (fullValuationChanged) body.full_valuation = fullValuation;
    updateSpace.mutate(body);
  };

  // What the selected country implies, from the server's list, visible before
  // saving; the session's rules stand in while it loads or for a country not in
  // it.
  const selectedRules =
    residencies.data?.find((r) => r.country === country) ??
    (country === session?.tax_residency
      ? session?.cost_basis_rules
      : undefined);

  // Sorted by the Russian name shown, not the ISO code; which countries are
  // offered is the server's.
  const countries = [...(residencies.data ?? [])].sort((a, b) =>
    countryName(a.country).localeCompare(countryName(b.country), "ru"),
  );

  // A country stored outside this form has no matching option, and Radix would
  // render an empty box. It is named and said to have unknown rules, as the server
  // says (MethodUnknown, PerimeterUnknown, unknown_country).
  const selectedIsOffered = countries.some(
    (rules) => rules.country === country,
  );

  return (
    <div className="grid gap-6">
      <h1 className="text-2xl font-bold">{t("settings.title")}</h1>
      <Card className="max-w-md">
        <CardContent className="grid gap-6">
          <div className="grid gap-2">
            <Label htmlFor="base-currency">{t("settings.baseCurrency")}</Label>
            <Select
              value={currency}
              onValueChange={(v) => {
                setCurrency(v);
                updateSpace.reset();
              }}
            >
              <SelectTrigger id="base-currency">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {COMMON_CURRENCIES.map((code) => (
                  <SelectItem key={code} value={code}>
                    {code}
                  </SelectItem>
                ))}
                <SelectItem value="custom">
                  {t("accounts.dialog.otherCurrency")}
                </SelectItem>
              </SelectContent>
            </Select>
            {currency === "custom" && (
              <Input
                aria-label={t("accounts.dialog.currencyPlaceholder")}
                placeholder={t("accounts.dialog.currencyPlaceholder")}
                value={customCurrency}
                maxLength={3}
                onChange={(e) => {
                  setCustomCurrency(e.target.value);
                  updateSpace.reset();
                }}
              />
            )}
            <p className="text-xs text-muted-foreground">
              {t("settings.baseCurrencyHint")}
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="tax-residency">{t("settings.taxResidency")}</Label>
            {residencies.isError ? (
              <Alert variant="destructive">
                <AlertDescription>{t("app.error")}</AlertDescription>
              </Alert>
            ) : (
              <Select
                value={country}
                disabled={countries.length === 0}
                onValueChange={(v) => {
                  setCountry(v);
                  updateSpace.reset();
                }}
              >
                <SelectTrigger id="tax-residency">
                  {/* Children override Radix's rendering, so they are passed only for
                     the unmatched country. */}
                  <SelectValue placeholder={t("app.loading")}>
                    {countries.length > 0 && !selectedIsOffered
                      ? t("settings.unknownCountry", {
                          country: countryName(country),
                        })
                      : undefined}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {countries.map((rules) => (
                    <SelectItem key={rules.country} value={rules.country}>
                      {countryName(rules.country)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
            <p className="text-xs text-muted-foreground">
              {t("settings.taxResidencyHint")}
            </p>
            {selectedRules && (
              <CostBasisNotice rules={selectedRules} confirmWhenSupported />
            )}
          </div>

          {/* Where the full valuation starts (decision Р-11); the «Итого» is
              the liquid one whatever is chosen here. */}
          <div className="grid gap-2">
            <Label htmlFor="full-valuation">{t("settings.fullValuation")}</Label>
            <Select
              value={fullValuation}
              onValueChange={(v) => {
                setFullValuation(v as FullValuation);
                updateSpace.reset();
              }}
            >
              <SelectTrigger id="full-valuation">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {FULL_VALUATIONS.map((v) => (
                  <SelectItem key={v} value={v}>
                    {t(`settings.fullValuations.${v}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{t("settings.fullValuationHint")}</p>
          </div>

          {updateSpace.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {updateSpace.error instanceof ApiError &&
                updateSpace.error.status === 403
                  ? t("settings.forbidden")
                  : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
          {/* Says the save happened: the fields already showed the new values,
             and a greyed Save button looks the same as an untouched form (#33).
             One neutral sentence for any combination of fields, not claiming the
             new figures are already elsewhere (caches are still refetching).
             role="status", not "alert": a confirmation, not a problem. Cleared
             when a field changes. */}
          {updateSpace.isSuccess && (
            <Alert role="status" data-testid="settings-saved">
              <AlertDescription>{t("settings.saved")}</AlertDescription>
            </Alert>
          )}
          <div>
            <Button disabled={!canSave} onClick={save}>
              {t("common.save")}
            </Button>
          </div>
        </CardContent>
      </Card>
      {/* The catalog follows the connections: imports create its rows, and
         this is the only place to correct one. */}
      <Card className="max-w-md">
        <CardHeader>
          <CardTitle>{t("instruments.title")}</CardTitle>
        </CardHeader>
        <CardContent>
          <Link
            to="/settings/instruments"
            className="text-sm underline"
            data-testid="settings-instruments-link"
          >
            {t("instruments.searchTitle")}
          </Link>
        </CardContent>
      </Card>
      <Card className="max-w-md">
        <CardHeader>
          <CardTitle>{t("categories.title")}</CardTitle>
        </CardHeader>
        <CardContent>
          <Link
            to="/settings/categories"
            className="text-sm underline"
            data-testid="settings-categories-link"
          >
            {t("categories.settingsLink")}
          </Link>
        </CardContent>
      </Card>
      <ConnectionsSection />
      <Card className="max-w-md">
        <CardHeader>
          <CardTitle>{t("dataSources.title")}</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-3">
          <p className="text-xs text-muted-foreground">{t("dataSources.hint")}</p>
          <DataSourcesList />
        </CardContent>
      </Card>
      <Card className="max-w-md">
        <CardHeader>
          <CardTitle>{t("settings.export.title")}</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-2">
          <a href="/api/v1/export" download className="text-sm underline" data-testid="settings-export-link">
            {t("settings.export.link")}
          </a>
          <p className="text-xs text-muted-foreground">{t("settings.export.hint")}</p>
        </CardContent>
      </Card>
    </div>
  );
}

// statusVariant: a switch over the contract's three values, so a fourth
// becomes a type error.
function statusVariant(
  status: TinvestConnectionStatus,
): "default" | "secondary" | "destructive" {
  switch (status) {
    case "active":
      return "default";
    case "token_revoked":
      return "destructive";
    case "disabled":
      return "secondary";
  }
}

// ConnectionsSection lists the broker connections and the way to add one,
// with its own query, so the form above failing does not blank it.
function ConnectionsSection() {
  const { t } = useTranslation();
  const connections = useConnections();
  const list = connections.data ?? [];
  const state = queryState(connections);

  return (
    <Card className="max-w-md">
      <CardHeader>
        <CardTitle>{t("connections.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <QueryGate state={state} />
        <RefreshFailedNotice show={refreshFailed(connections)} />
        {state === "ready" &&
          list.length === 0 && (
            <p className="text-sm text-muted-foreground">
              {t("connections.empty")}
            </p>
          )}
        {list.length > 0 && (
          <ul className="grid gap-2">
            {list.map((connection) => (
              <li key={connection.id}>
                <Link
                  to="/settings/connections/$connectionId"
                  params={{ connectionId: connection.id }}
                  className="flex items-center justify-between rounded-lg border p-3 text-sm hover:bg-muted"
                >
                  <div className="grid gap-0.5">
                    <span className="font-medium">
                      {t("connections.tinvest")}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      {t("connections.tokenLast4", {
                        last4: connection.token_last4,
                      })}
                    </span>
                  </div>
                  {/* Three looks for three states: token_revoked waits for the owner to
                     paste a token, disabled for nobody. */}
                  <Badge variant={statusVariant(connection.status)}>
                    {t(`connections.statuses.${connection.status}`)}
                  </Badge>
                </Link>
              </li>
            ))}
          </ul>
        )}
        <div>
          <Button asChild>
            <Link to="/settings/connections/new">
              {t("connections.connect")}
            </Link>
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
