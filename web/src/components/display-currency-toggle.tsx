import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { useDisplayCurrency } from "@/lib/display-currency";
import { cn } from "@/lib/utils";

// Compact segmented control for the header: switches every money amount on
// screen between the currency that amount is itself denominated in ("native")
// and the space's base currency ("base", via the backend's `in_base` figures).
//
// WHAT "native" IS, and why the label no longer says «в валюте счёта» (#108).
// This control governs three screens and only one of them shows accounts. A
// position row's figures are in the POSITION's currency — or a quote's, or a
// bond face value's — and a journal row's are in the OPERATION's, and neither
// has to be the account's: the demo stand's ruble Т-Банк account holds dollar
// rows, so the toggle offered to show them «в валюте счёта» while showing
// dollars. «в исходной валюте» is true of every row on all three screens, and
// it is the phrase the unconverted captions on the positions screen already
// use for the same thing.
//
// Built
// from the existing Button component (variant swap for the active segment)
// rather than a new UI primitive — there's no dedicated segmented-control
// component in this project yet, and one isn't warranted for a single use.
//
// `visible` lets the parent hide the control entirely: it only makes sense
// when more than one currency is actually shown on the current screen, and
// that determination belongs to the screens themselves (a later task), not
// to this component or to the app-wide header that renders it.
export function DisplayCurrencyToggle({ visible }: { visible: boolean }) {
  const { t } = useTranslation();
  const { mode, setMode } = useDisplayCurrency();

  if (!visible) return null;

  return (
    <div
      role="group"
      title={t("displayCurrency.hint")}
      aria-label={t("displayCurrency.hint")}
      className="inline-flex items-center gap-0.5 rounded-lg border p-0.5"
    >
      <Button
        type="button"
        variant={mode === "native" ? "secondary" : "ghost"}
        size="xs"
        aria-pressed={mode === "native"}
        className={cn(mode !== "native" && "text-muted-foreground")}
        onClick={() => setMode("native")}
      >
        {t("displayCurrency.native")}
      </Button>
      <Button
        type="button"
        variant={mode === "base" ? "secondary" : "ghost"}
        size="xs"
        aria-pressed={mode === "base"}
        className={cn(mode !== "base" && "text-muted-foreground")}
        onClick={() => setMode("base")}
      >
        {t("displayCurrency.base")}
      </Button>
    </div>
  );
}
