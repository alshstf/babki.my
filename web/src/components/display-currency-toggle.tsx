import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { useDisplayCurrency } from "@/lib/display-currency";
import { cn } from "@/lib/utils";

// The header's segmented control: each amount in its own ("native") currency or
// in the base currency. «в исходной валюте», not «в валюте счёта» (#108): rows are
// in a position's, quote's, face's or operation's currency, not necessarily the
// account's. Built from Button. `visible` is the parent's: only screens know how
// many currencies they show.
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
