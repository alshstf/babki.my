import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { countryName } from "@/lib/country";
import type { CostBasisRules } from "@/api/tax-residencies";

// costBasisCaveat is CostBasisNotice's statement as one tooltip string, for a
// single money cell whose figure is a cost basis (a transferred parcel in the
// journal). It opens by saying what the figure is, then the notice's sentences.
// Undefined when the country's rule is what is computed.
export function costBasisCaveat(
  t: (key: string, opts?: Record<string, string>) => string,
  rules: CostBasisRules,
): string | undefined {
  if (rules.supported) return undefined;
  return [
    t("costBasis.figureIsACostBasis"),
    t("costBasis.residency", { country: countryName(rules.country) }),
    ...rules.notices.map((notice) => t(`costBasis.notices.${notice}`)),
  ].join("\n");
}

// CostBasisNotice says whether the cost basis on screen is the one the owner's
// country requires and, when not, every way it differs. The application always
// releases earliest purchases first per account; whether that is the country's
// rule is the server's table (internal/family/taxresidency.go), published as
// `supported` and `notices`, and this only turns the codes into sentences. The
// visible text gives the consequence; the mechanics are in the tooltip.
export function CostBasisNotice({
  rules,
  // Whether to name the country: the sentences say «в этой стране», which the
  // positions screen needs a referent for; settings shows it beside the
  // selector.
  namesCountry = false,
  // Whether to speak when there is nothing to warn about: settings does,
  // right after the country is picked; everywhere else a permanent banner would
  // drown real warnings.
  confirmWhenSupported = false,
}: {
  rules: CostBasisRules;
  namesCountry?: boolean;
  confirmWhenSupported?: boolean;
}) {
  const { t } = useTranslation();
  if (rules.supported && !confirmWhenSupported) return null;
  // The mechanics in the tooltip, with method and perimeter translated (and
  // checked by i18n:check), never shown as wire codes.
  const title =
    t("costBasis.howWeCompute") +
    "\n" +
    t("costBasis.countryRule", {
      country: countryName(rules.country),
      method: t(`costBasis.methods.${rules.method}`),
      perimeter: t(`costBasis.perimeters.${rules.perimeter}`),
    });
  return (
    <Alert data-testid="cost-basis-notice" title={title}>
      <Info />
      <AlertDescription className="grid gap-1">
        {namesCountry && (
          <span className="font-medium">
            {t("costBasis.residency", { country: countryName(rules.country) })}
          </span>
        )}
        {rules.supported ? (
          <span>{t("costBasis.supported")}</span>
        ) : (
          rules.notices.map((notice) => (
            <span key={notice}>{t(`costBasis.notices.${notice}`)}</span>
          ))
        )}
      </AlertDescription>
    </Alert>
  );
}
