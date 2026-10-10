import { useRef, useState } from "react";
import { Upload } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useImportReceipts, useWaitingReceipts, type ReceiptImportResult } from "@/api/receipts";
import { useSession } from "@/api/session";
import { formatDate } from "@/lib/dates";
import { formatMinor } from "@/lib/money";

// ReceiptsCard takes a statement of the tax service's app «Проверка чеков»
// (decision Р-34) and lists the receipts still waiting for a row. A viewer
// sees the waiting ones only, and nothing while there are none.
export function ReceiptsCard() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const canEdit = session?.role === "owner" || session?.role === "editor";
  const waiting = useWaitingReceipts();
  const importReceipts = useImportReceipts();
  const file = useRef<HTMLInputElement>(null);
  const [badFile, setBadFile] = useState(false);
  const [result, setResult] = useState<ReceiptImportResult | null>(null);
  const list = waiting.data ?? [];
  if (!canEdit && list.length === 0) return null;

  const take = async (picked: File | undefined) => {
    if (!picked) return;
    setBadFile(false);
    setResult(null);
    let statement: unknown;
    try {
      statement = JSON.parse(await picked.text());
    } catch {
      setBadFile(true);
      return;
    }
    importReceipts.mutate(statement, { onSuccess: setResult });
  };

  return (
    <Card data-testid="money-receipts">
      <CardHeader>
        <CardTitle>{t("receipts.title")}</CardTitle>
        <p className="text-sm text-muted-foreground">{t("receipts.hint")}</p>
      </CardHeader>
      <CardContent className="grid grid-cols-1 gap-3">
        {canEdit && (
          <div className="grid gap-1">
            <input
              ref={file}
              type="file"
              accept="application/json,.json"
              className="hidden"
              data-testid="receipts-file"
              onChange={(e) => {
                void take(e.target.files?.[0]);
                e.target.value = "";
              }}
            />
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="justify-self-start"
              disabled={importReceipts.isPending}
              onClick={() => file.current?.click()}
            >
              <Upload className="size-4" />
              {importReceipts.isPending ? t("receipts.loading") : t("receipts.load")}
            </Button>
            <p className="text-xs text-muted-foreground">{t("receipts.how")}</p>
          </div>
        )}
        {badFile && (
          <Alert variant="destructive">
            <AlertDescription>{t("receipts.badFile")}</AlertDescription>
          </Alert>
        )}
        {importReceipts.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("receipts.failed")}</AlertDescription>
          </Alert>
        )}
        {result && (
          <p className="text-sm" data-testid="receipts-result">
            {result.found === 0
              ? t("receipts.none")
              : [
                  t("receipts.found", { n: result.found }),
                  result.attached > 0 && t("receipts.attached", { n: result.attached }),
                  result.enriched > 0 && t("receipts.enriched", { n: result.enriched }),
                  result.split > 0 && t("receipts.split", { n: result.split }),
                  result.waiting > 0 && t("receipts.waitingCount", { n: result.waiting }),
                  result.known > 0 && t("receipts.known", { n: result.known }),
                ]
                  .filter(Boolean)
                  .join(" ")}
          </p>
        )}
        {list.length > 0 && (
          <div className="grid gap-1" data-testid="receipts-waiting">
            <p className="text-sm font-medium">{t("receipts.waitingTitle", { n: list.length })}</p>
            <p className="text-xs text-muted-foreground">{t("receipts.waitingHint")}</p>
            <ul className="grid gap-0.5 text-sm">
              {list.slice(0, 20).map((r) => (
                <li key={r.id} className="flex flex-wrap justify-between gap-2" data-testid="receipt-waiting">
                  <span className="min-w-0 truncate">
                    {formatDate(r.issued_at.slice(0, 10))} {r.issued_at.slice(11, 16)} · {r.seller ?? t("receipts.noSeller")}
                  </span>
                  <span className="tabular-nums">{formatMinor(r.total_minor, "RUB")}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
