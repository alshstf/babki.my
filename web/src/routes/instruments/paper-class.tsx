import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useInstrumentClass, useSetInstrumentClass, type InstrumentClass } from "@/api/instrument-page";
import { ApiError } from "@/api/operations";
import { countryName } from "@/lib/country";
import { sectorName } from "@/lib/sectors";

// PaperClass is the paper's country of risk and sector (decision Р-46), where
// each comes from, and — for an editor — the family's own over the source's.
export function PaperClass({ instrumentId, canEdit }: { instrumentId: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const q = useInstrumentClass(instrumentId);
  const [editing, setEditing] = useState(false);
  const c = q.data;
  if (!c) return null;
  const from = (byHand: boolean) =>
    byHand
      ? t("instrumentPage.classByHand")
      : c.source === "tinvest"
        ? t("instrumentPage.classFromTinvest")
        : c.source === "yahoo"
          ? t("instrumentPage.classFromYahoo")
          : t("instrumentPage.classFromNone");
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-sm" data-testid="instrument-class">
      <span>
        {t("instrumentPage.country")}: <b>{c.country ? countryName(c.country) : t("instrumentPage.unknown")}</b>{" "}
        <span className="text-muted-foreground">· {c.country || c.country_by_hand ? from(c.country_by_hand) : t("instrumentPage.countryByIsin")}</span>
      </span>
      <span>
        {t("instrumentPage.sector")}: <b>{c.sector ? sectorName(t, c.sector) : t("instrumentPage.unknown")}</b>{" "}
        <span className="text-muted-foreground">· {from(c.sector_by_hand)}</span>
      </span>
      {canEdit && (
        <Button variant="link" className="h-auto p-0 text-sm" onClick={() => setEditing(true)}>
          {t("common.edit")}
        </Button>
      )}
      {editing && <ClassDialog instrumentId={instrumentId} current={c} onClose={() => setEditing(false)} />}
    </div>
  );
}

const SOURCE = "source";

function ClassDialog({ instrumentId, current, onClose }: { instrumentId: string; current: InstrumentClass; onClose: () => void }) {
  const { t } = useTranslation();
  const [country, setCountry] = useState(current.country_by_hand ? current.country : "");
  const [sector, setSector] = useState(current.sector_by_hand ? current.sector : SOURCE);
  const save = useSetInstrumentClass(instrumentId);
  const code = country.trim().toUpperCase();
  const countryOk = code === "" || /^[A-Z]{2}$/.test(code);
  const submit = (body: { country: string | null; sector: string | null }) => save.mutate(body, { onSuccess: onClose });
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("instrumentPage.classTitle")}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <div className="grid gap-1">
            <Label htmlFor="class-country">{t("instrumentPage.countryCode")}</Label>
            <Input id="class-country" className="w-24" maxLength={2} value={country} onChange={(e) => setCountry(e.target.value)} />
            <p className="text-xs text-muted-foreground">
              {code === ""
                ? t("instrumentPage.keepSource", { value: current.source_country ? countryName(current.source_country) : t("instrumentPage.unknown") })
                : countryOk
                  ? countryName(code)
                  : t("instrumentPage.countryBad")}
            </p>
          </div>
          <div className="grid gap-1">
            <Label htmlFor="class-sector">{t("instrumentPage.sector")}</Label>
            <Select value={sector} onValueChange={setSector}>
              <SelectTrigger id="class-sector" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={SOURCE}>
                  {t("instrumentPage.keepSource", { value: current.source_sector ? sectorName(t, current.source_sector) : t("instrumentPage.unknown") })}
                </SelectItem>
                {current.sectors.map((s) => (
                  <SelectItem key={s} value={s}>
                    {sectorName(t, s)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {current.raw && <p className="text-xs text-muted-foreground">{t("instrumentPage.sourceSaid", { raw: current.raw })}</p>}
          </div>
          {save.isError && (
            <p className="text-sm text-destructive">
              {save.error instanceof ApiError && save.error.status === 400 ? t("instrumentPage.countryBad") : t("instrumentPage.classFailed")}
            </p>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button
            disabled={!countryOk || save.isPending}
            onClick={() => submit({ country: code === "" ? null : code, sector: sector === SOURCE ? null : sector })}
          >
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
