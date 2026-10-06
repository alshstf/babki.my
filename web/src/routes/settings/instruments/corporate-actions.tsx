import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  useCreateInstrumentEvent,
  useDeleteInstrumentEvent,
  useInstrumentEvents,
  type CreateInstrumentEventBody,
  type InstrumentEventKind,
} from "@/api/instrument-events";
import { MAX_EVENT_NOTE, MAX_EVENT_SOURCE } from "@/lib/text-limits";

// The registry of what happened to papers, and a person's door into it. The
// exchange's splits arrive daily by themselves; foreign splits, conversions and
// spin-offs are known only to the owner. On the catalog screen because it is
// also a fact about the security, instance-wide.
export function CorporateActions({ canEdit }: { canEdit: boolean }) {
  const { t } = useTranslation();
  const events = useInstrumentEvents();
  const create = useCreateInstrumentEvent();
  const remove = useDeleteInstrumentEvent();
  const [open, setOpen] = useState(false);

  const [kind, setKind] = useState<InstrumentEventKind>("split");
  const [isin, setIsin] = useState("");
  const [effectiveOn, setEffectiveOn] = useState("");
  const [ratioFrom, setRatioFrom] = useState("1");
  const [ratioTo, setRatioTo] = useState("");
  const [resultIsin, setResultIsin] = useState("");
  // 0 by default: the broker keeps a carve-out at no cost (decision Р-16).
  const [basisShare, setBasisShare] = useState("0");
  const [sourceRef, setSourceRef] = useState("");
  const [note, setNote] = useState("");

  const rows = events.data ?? [];

  function reset() {
    setKind("split");
    setIsin("");
    setEffectiveOn("");
    setRatioFrom("1");
    setRatioTo("");
    setResultIsin("");
    setBasisShare("");
    setSourceRef("");
    setNote("");
  }

  function submit() {
    const body: CreateInstrumentEventBody = {
      kind,
      isin: isin.trim().toUpperCase(),
      effective_on: effectiveOn,
      ratio_from: Number(ratioFrom),
      ratio_to: Number(ratioTo),
      source_ref: sourceRef.trim(),
    };
    // Sent only where the kind means something by them; the server refuses
    // either field on a kind that has no use for it, and sending an empty one
    // would turn a form the person left alone into a refusal they did not earn.
    if (kind !== "split") body.result_isin = resultIsin.trim().toUpperCase();
    if (kind === "spin_off") body.basis_share = basisShare.trim();
    if (note.trim() !== "") body.note = note.trim();
    create.mutate(body, {
      onSuccess: () => {
        reset();
        setOpen(false);
      },
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("corporateActions.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <p className="text-sm text-muted-foreground">
          {t("corporateActions.intro")}
        </p>

        {events.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("app.error")}</AlertDescription>
          </Alert>
        )}
        {events.isPending && (
          <p className="text-sm text-muted-foreground">{t("app.loading")}</p>
        )}
        {!events.isPending && !events.isError && rows.length === 0 && (
          <p
            className="text-sm text-muted-foreground"
            data-testid="corporate-actions-empty"
          >
            {t("corporateActions.empty")}
          </p>
        )}

        <ul className="grid gap-2">
          {rows.map((event) => (
            <li
              key={event.id}
              data-testid="corporate-action-row"
              className="flex flex-wrap items-center justify-between gap-2 rounded-md border p-3"
            >
              <div className="grid gap-0.5">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">
                    {t(`corporateActions.kinds.${event.kind}`)}
                  </span>
                  <span className="tabular-nums">
                    {event.ratio_from}:{event.ratio_to}
                  </span>
                  <span className="tabular-nums text-muted-foreground">
                    {event.effective_on}
                  </span>
                  {/* Two answers from the server: `materialized` is about the kind (none
                     unapplicable today), `not_counted_reason` about this event (its
                     produced paper not catalogued). */}
                  {!event.materialized && (
                    <Badge variant="outline">
                      {t("corporateActions.notCounted")}
                    </Badge>
                  )}
                  {event.materialized && event.not_counted_reason != null && (
                    <Badge variant="outline">
                      {t(`corporateActions.notCountedReasons.${event.not_counted_reason}`)}
                    </Badge>
                  )}
                </div>
                <div className="text-xs text-muted-foreground">
                  {[event.isin, event.result_isin ?? ""]
                    .filter((part) => part !== "")
                    .join(" → ")}
                </div>
                <div className="text-xs text-muted-foreground">
                  {event.source === "moex_iss"
                    ? t("corporateActions.sources.moex_iss")
                    : event.source === "yahoo"
                      ? t("corporateActions.sources.yahoo")
                      : t("corporateActions.sources.manual")}
                  {event.source_ref !== "" && (
                    <>
                      {" · "}
                      <a
                        className="underline"
                        href={event.source_ref}
                        target="_blank"
                        rel="noreferrer"
                        data-testid="corporate-action-source"
                      >
                        {t("corporateActions.evidence")}
                      </a>
                    </>
                  )}
                </div>
                {event.note !== "" && (
                  <div className="text-xs text-muted-foreground">{event.note}</div>
                )}
              </div>
              {/* No delete for an exchange row: the job would write it back; the
                 server refuses it too (400). */}
              {canEdit && event.source === "manual" && (
                <Button
                  variant="outline"
                  size="sm"
                  data-testid={`corporate-action-delete-${event.id}`}
                  disabled={remove.isPending}
                  onClick={() => remove.mutate(event.id)}
                >
                  {t("common.delete")}
                </Button>
              )}
            </li>
          ))}
        </ul>

        {remove.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("corporateActions.deleteFailed")}</AlertDescription>
          </Alert>
        )}

        {canEdit && !open && (
          <Button
            variant="outline"
            data-testid="corporate-action-add"
            onClick={() => setOpen(true)}
          >
            {t("corporateActions.add")}
          </Button>
        )}

        {canEdit && open && (
          <form
            className="grid gap-3 rounded-md border p-3"
            data-testid="corporate-action-form"
            onSubmit={(e) => {
              e.preventDefault();
              submit();
            }}
          >
            <div className="grid gap-1.5">
              <Label htmlFor="ca-kind">{t("corporateActions.fields.kind")}</Label>
              <Select
                value={kind}
                onValueChange={(v) => setKind(v as InstrumentEventKind)}
              >
                <SelectTrigger id="ca-kind">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="split">
                    {t("corporateActions.kinds.split")}
                  </SelectItem>
                  <SelectItem value="conversion">
                    {t("corporateActions.kinds.conversion")}
                  </SelectItem>
                  <SelectItem value="spin_off">
                    {t("corporateActions.kinds.spin_off")}
                  </SelectItem>
                </SelectContent>
              </Select>
            </div>

            <div className="grid gap-1.5">
              <Label htmlFor="ca-isin">{t("corporateActions.fields.isin")}</Label>
              <Input
                id="ca-isin"
                value={isin}
                onChange={(e) => setIsin(e.target.value)}
              />
            </div>

            {kind !== "split" && (
              <div className="grid gap-1.5">
                <Label htmlFor="ca-result-isin">
                  {t("corporateActions.fields.resultIsin")}
                </Label>
                <Input
                  id="ca-result-isin"
                  value={resultIsin}
                  onChange={(e) => setResultIsin(e.target.value)}
                />
              </div>
            )}

            <div className="grid gap-1.5">
              <Label htmlFor="ca-date">{t("corporateActions.fields.date")}</Label>
              <Input
                id="ca-date"
                type="date"
                value={effectiveOn}
                onChange={(e) => setEffectiveOn(e.target.value)}
              />
              {/* THE ONE FIELD A PERSON CAN GET WRONG WITHOUT NOTICING, so the
                  hint states which day it wants and gives the case where the
                  two are not the same day at all. */}
              <p className="text-xs text-muted-foreground">
                {t("corporateActions.fields.dateHint")}
              </p>
            </div>

            <div className="grid gap-1.5">
              <Label htmlFor="ca-ratio-from">
                {t("corporateActions.fields.ratio")}
              </Label>
              <div className="flex items-center gap-2">
                <Input
                  id="ca-ratio-from"
                  inputMode="numeric"
                  className="w-24"
                  value={ratioFrom}
                  onChange={(e) => setRatioFrom(e.target.value)}
                />
                <span aria-hidden="true">:</span>
                <Input
                  id="ca-ratio-to"
                  inputMode="numeric"
                  className="w-24"
                  aria-label={t("corporateActions.fields.ratioTo")}
                  value={ratioTo}
                  onChange={(e) => setRatioTo(e.target.value)}
                />
              </div>
              <p className="text-xs text-muted-foreground">
                {t("corporateActions.fields.ratioHint")}
              </p>
            </div>

            {kind === "spin_off" && (
              <div className="grid gap-1.5">
                <Label htmlFor="ca-basis-share">
                  {t("corporateActions.fields.basisShare")}
                </Label>
                <Input
                  id="ca-basis-share"
                  inputMode="decimal"
                  value={basisShare}
                  onChange={(e) => setBasisShare(e.target.value)}
                />
                <p className="text-xs text-muted-foreground">
                  {t("corporateActions.fields.basisShareHint")}
                </p>
              </div>
            )}

            <div className="grid gap-1.5">
              <Label htmlFor="ca-source-ref">
                {t("corporateActions.fields.sourceRef")}
              </Label>
              <Input
                id="ca-source-ref"
                maxLength={MAX_EVENT_SOURCE}
                value={sourceRef}
                onChange={(e) => setSourceRef(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                {t("corporateActions.fields.sourceRefHint")}
              </p>
            </div>

            <div className="grid gap-1.5">
              <Label htmlFor="ca-note">{t("corporateActions.fields.note")}</Label>
              <Input
                id="ca-note"
                maxLength={MAX_EVENT_NOTE}
                value={note}
                onChange={(e) => setNote(e.target.value)}
              />
            </div>

            {create.isError && (
              <Alert variant="destructive">
                <AlertDescription>
                  {t("corporateActions.createFailed")}
                </AlertDescription>
              </Alert>
            )}

            <div className="flex gap-2">
              <Button type="submit" disabled={create.isPending}>
                {t("common.save")}
              </Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => {
                  reset();
                  setOpen(false);
                }}
              >
                {t("common.cancel")}
              </Button>
            </div>
          </form>
        )}
      </CardContent>
    </Card>
  );
}
