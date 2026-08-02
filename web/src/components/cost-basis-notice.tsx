import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { countryName } from "@/lib/country";
import type { CostBasisRules } from "@/api/tax-residencies";

// States, in Russian, whether the cost basis behind the figures on screen is
// the one the owner's country actually requires — and, when it is not, every
// way in which it is not.
//
// The application always computes one rule: the earliest purchases are
// released first, queued per account. Whether that rule IS the country's is
// decided by a table in the server (internal/family/taxresidency.go) and
// published on the session and on the positions response as `supported` plus
// a list of `notices`. This component is the whole of the interface's part in
// that: it turns those codes into sentences and adds nothing of its own. The
// SET of divergences is the server's to define — a new kind of notice appears
// in ru.json (npm run i18n:check fails until it does), never as a condition
// invented here.
//
// Everything technical stays in the tooltip, never in the visible text: the
// method and perimeter codes, and the sentence naming exactly what the
// application computes. That is the owner's standing rule about dates and
// internals being visual noise — the reader gets the consequence, and the
// mechanics only if they ask for them.
export function CostBasisNotice({
  rules,
  // Whether to name the country above the sentences. The sentences say "в
  // этой стране", which needs a referent: the positions screen never
  // mentions the residency otherwise, so it names it; the settings screen
  // shows this directly under the country selector, where repeating it is
  // noise.
  namesCountry = false,
  // Whether to also say something when there is nothing to warn about. The
  // settings screen does: the owner just picked a country and the answer to
  // "what does that mean for my figures" must be visible either way, and
  // "our computation is this country's rule" is a claim the server actually
  // makes (see TaxRules.Supported). Every other screen stays silent when
  // supported — a banner on every visit for a reader with nothing to be
  // warned about is the noise that makes real warnings invisible.
  confirmWhenSupported = false,
}: {
  rules: CostBasisRules;
  namesCountry?: boolean;
  confirmWhenSupported?: boolean;
}) {
  const { t } = useTranslation();
  if (rules.supported && !confirmWhenSupported) return null;
  // The mechanics, disclosed on demand rather than as text: what the
  // application computes, plus the country's own method and perimeter. Both
  // enums are translated rather than printed as the codes they travel as —
  // "average"/"owner" in a tooltip would be the interface speaking the wire
  // format at the reader — and both are cross-checked against the generated
  // schema by npm run i18n:check, so a value the server adds cannot show up
  // here as a blank.
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
