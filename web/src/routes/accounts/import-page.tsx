import { useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useAccounts } from "@/api/accounts";
import { useInstrumentIndex } from "@/api/instruments";
import { isConflict, type OperationType } from "@/api/operations";
import {
  useImportTable,
  usePreviewImport,
  useRollBackImport,
  useTableImports,
  type ImportField,
  type ImportMapping,
  type ImportPreview,
  type ImportRow,
} from "@/api/imports";
import { formatMinor } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { readTableFile } from "@/lib/read-table-file";
import { cn } from "@/lib/utils";

// The fields a column can mean, in the order a person reads a trade.
const FIELDS: ImportField[] = [
  "date",
  "type",
  "instrument",
  "quantity",
  "price",
  "amount",
  "currency",
  "fee",
  "note",
];

// The operations a table may hold (the server refuses any other).
const TYPES: OperationType[] = [
  "buy",
  "sell",
  "deposit",
  "withdrawal",
  "dividend",
  "coupon",
  "interest",
  "tax",
  "fee",
  "amortization",
];

const NONE = "none";

const VERDICT_CLASS: Record<ImportRow["verdict"], string> = {
  new: "text-emerald-700 border-emerald-600/40",
  duplicate: "text-muted-foreground",
  unparsed: "text-amber-700 border-amber-600/40",
  refused: "text-red-700 border-red-600/40",
};

// The values the type column holds, as the server compares them: ignoring
// case and surrounding blanks.
function typeValues(rows: ImportRow[], column: number | undefined): string[] {
  if (column === undefined) return [];
  const seen = new Set<string>();
  for (const row of rows) {
    const value = (row.cells[column] ?? "").trim().toLowerCase();
    if (value !== "") seen.add(value);
  }
  return [...seen].sort();
}

export function ImportPage() {
  const { accountId } = useParams({ from: "/app/accounts/$accountId/import" });
  return <TableImport accountId={accountId} />;
}

export function TableImport({ accountId }: { accountId: string }) {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const account = accounts.data?.find((a) => a.id === accountId);
  const preview = usePreviewImport(accountId);
  const importTable = useImportTable(accountId);
  const [content, setContent] = useState<string | null>(null);
  const [fileName, setFileName] = useState("");
  const [current, setCurrent] = useState<ImportPreview | null>(null);

  const ask = (body: { content: string; mapping?: ImportMapping }) => {
    importTable.reset();
    preview.mutate(body, { onSuccess: setCurrent });
  };

  const chooseFile = async (file: File | undefined) => {
    if (!file) return;
    const text = await readTableFile(file);
    setContent(text);
    setFileName(file.name);
    setCurrent(null);
    ask({ content: text });
  };

  const remap = (mapping: ImportMapping) => {
    if (content === null) return;
    setCurrent((c) => (c ? { ...c, mapping } : c));
    ask({ content, mapping });
  };

  const counts = { new: 0, duplicate: 0, unparsed: 0, refused: 0 };
  for (const row of current?.rows ?? []) counts[row.verdict]++;

  const runImport = () => {
    if (content === null || !current) return;
    importTable.mutate(
      { content, mapping: current.mapping, file_name: fileName },
      { onSuccess: (result) => setCurrent({ ...current, rows: result.rows }) },
    );
  };

  return (
    <div className="grid gap-6">
      <Link
        to="/accounts/$accountId"
        params={{ accountId }}
        className="text-sm text-muted-foreground hover:underline"
      >
        ← {account?.name ?? t("accounts.title")}
      </Link>
      <div className="grid gap-1">
        <h1 className="text-2xl font-bold">{t("tableImport.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("tableImport.lead")}</p>
      </div>

      <div className="grid max-w-md gap-2">
        <Label htmlFor="import-file">{t("tableImport.file")}</Label>
        <Input
          id="import-file"
          type="file"
          accept=".csv,.txt,text/csv,text/plain"
          onChange={(e) => void chooseFile(e.target.files?.[0])}
        />
        <p className="text-xs text-muted-foreground">{t("tableImport.fileHint")}</p>
      </div>

      {preview.isError && (
        <Alert variant="destructive">
          <AlertDescription>{t("tableImport.unreadable")}</AlertDescription>
        </Alert>
      )}

      {current && (
        <MappingEditor preview={current} onChange={remap} disabled={preview.isPending} />
      )}

      {current && (
        <div className="grid gap-3">
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
            <span data-testid="import-count-new">
              {importTable.isSuccess
                ? t("tableImport.countWritten", { count: counts.new })
                : t("tableImport.countNew", { count: counts.new })}
            </span>
            <span className="text-muted-foreground">
              {t("tableImport.countDuplicate", { count: counts.duplicate })}
            </span>
            <span className="text-amber-700">
              {t("tableImport.countUnparsed", { count: counts.unparsed })}
            </span>
            <span className="text-red-700">
              {t("tableImport.countRefused", { count: counts.refused })}
            </span>
          </div>
          {importTable.isSuccess ? (
            <Alert>
              <AlertDescription data-testid="import-done">
                {importTable.data.import
                  ? t("tableImport.done", { count: importTable.data.import.rows_written })
                  : t("tableImport.nothingNew")}
              </AlertDescription>
            </Alert>
          ) : (
            <div>
              <Button
                disabled={counts.new === 0 || preview.isPending || importTable.isPending}
                onClick={runImport}
              >
                {t("tableImport.run", { count: counts.new })}
              </Button>
            </div>
          )}
          {importTable.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(importTable.error) ? t("operations.conflict") : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
          <PreviewTable rows={current.rows} written={importTable.isSuccess} />
        </div>
      )}

      <ImportsList accountId={accountId} />
    </div>
  );
}

function MappingEditor({
  preview,
  onChange,
  disabled,
}: {
  preview: ImportPreview;
  onChange: (mapping: ImportMapping) => void;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const { mapping } = preview;
  const width = Math.max(
    preview.header.length,
    ...preview.rows.map((row) => row.cells.length),
  );
  const columnName = (i: number) =>
    preview.header[i] ? preview.header[i] : t("tableImport.column", { n: i + 1 });
  const setColumn = (field: ImportField, value: string) => {
    const columns = { ...mapping.columns };
    if (value === NONE) delete columns[field];
    else columns[field] = Number(value);
    onChange({ ...mapping, columns });
  };
  const setType = (cell: string, value: string) => {
    const types = { ...mapping.types };
    if (value === NONE) delete types[cell];
    else types[cell] = value as OperationType;
    onChange({ ...mapping, types });
  };
  const values = typeValues(preview.rows, mapping.columns.type);

  return (
    <div className="grid gap-4 rounded-lg border p-4">
      <div className="flex items-center gap-2">
        <Checkbox
          id="import-header"
          checked={mapping.has_header}
          disabled={disabled}
          onCheckedChange={(checked) => onChange({ ...mapping, has_header: checked === true })}
        />
        <Label htmlFor="import-header">{t("tableImport.hasHeader")}</Label>
      </div>
      <div className="grid gap-3 sm:grid-cols-3">
        {FIELDS.map((field) => (
          <div key={field} className="grid gap-1">
            <Label>{t(`tableImport.fields.${field}`)}</Label>
            <Select
              value={mapping.columns[field] === undefined ? NONE : String(mapping.columns[field])}
              onValueChange={(v) => setColumn(field, v)}
              disabled={disabled}
            >
              <SelectTrigger aria-label={t(`tableImport.fields.${field}`)}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={NONE}>{t("tableImport.noColumn")}</SelectItem>
                {Array.from({ length: width }, (_, i) => (
                  <SelectItem key={i} value={String(i)}>
                    {columnName(i)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        ))}
      </div>
      {values.length > 0 && (
        <div className="grid gap-2">
          <div className="text-sm font-medium">{t("tableImport.typeValues")}</div>
          <div className="grid gap-2 sm:grid-cols-3">
            {values.map((value) => (
              <div key={value} className="grid gap-1">
                <Label className="font-normal text-muted-foreground">«{value}»</Label>
                <Select
                  value={mapping.types[value] ?? NONE}
                  onValueChange={(v) => setType(value, v)}
                  disabled={disabled}
                >
                  <SelectTrigger aria-label={value}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={NONE}>{t("tableImport.skipType")}</SelectItem>
                    {TYPES.map((type) => (
                      <SelectItem key={type} value={type}>
                        {t(`operationTypes.${type}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// After the import, a row that was to be written has been: it says so.
function PreviewTable({ rows, written }: { rows: ImportRow[]; written: boolean }) {
  const { t } = useTranslation();
  const instruments = useInstrumentIndex();
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("tableImport.line")}</TableHead>
          <TableHead>{t("tableImport.verdict")}</TableHead>
          <TableHead>{t("operations.columns.date")}</TableHead>
          <TableHead>{t("operations.columns.type")}</TableHead>
          <TableHead>{t("operations.columns.instrument")}</TableHead>
          <TableHead className="text-right">{t("operations.columns.amount")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => {
          const op = row.operation;
          const paper = op?.instrument_id ? instruments.get(op.instrument_id) : undefined;
          return (
            <TableRow key={row.line} data-testid={`import-row-${row.line}`}>
              <TableCell className="text-muted-foreground">{row.line}</TableCell>
              <TableCell className="whitespace-normal">
                <Badge variant="outline" className={cn(VERDICT_CLASS[row.verdict])}>
                  {written && row.verdict === "new"
                    ? t("tableImport.writtenVerdict")
                    : t(`tableImport.verdicts.${row.verdict}`)}
                </Badge>
                {row.reason && <ReasonText reason={row.reason} />}
              </TableCell>
              <TableCell>{op ? formatDate(op.occurred_on) : "—"}</TableCell>
              <TableCell>{op ? t(`operationTypes.${op.type}`) : row.cells.join(" · ")}</TableCell>
              <TableCell>
                {paper ? paper.name : "—"}
                {op?.quantity && op.price && (
                  <div className="text-xs text-muted-foreground">
                    {op.quantity} × {op.price}
                  </div>
                )}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {op ? formatMinor(op.amount_minor, op.currency) : "—"}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

// Why a row is not imported, in words. The cell's own text is quoted as it
// stands in the file; the journal's refusal is carried untranslated under the
// sentence, as the broker import's list carries it.
function ReasonText({ reason }: { reason: NonNullable<ImportRow["reason"]> }) {
  const { t } = useTranslation();
  const field = reason.field ? t(`tableImport.fields.${reason.field}`) : "";
  return (
    <div className="mt-1 grid max-w-80 gap-0.5 text-xs">
      <span>{t(`tableImport.reasons.${reason.code}`, { field, value: reason.value })}</span>
      {reason.code === "engine_refused" && (
        <span className="text-muted-foreground">{reason.value}</span>
      )}
    </div>
  );
}

function ImportsList({ accountId }: { accountId: string }) {
  const { t } = useTranslation();
  const imports = useTableImports(accountId);
  const rollBack = useRollBackImport(accountId);
  // Taking an import back removes every operation it wrote, so it is asked
  // about first.
  const [target, setTarget] = useState<string | null>(null);
  if (!imports.data || imports.data.length === 0) return null;
  return (
    <div className="grid gap-2">
      <h2 className="text-lg font-semibold">{t("tableImport.history")}</h2>
      {rollBack.isError && (
        <Alert variant="destructive">
          <AlertDescription>
            {isConflict(rollBack.error) ? t("tableImport.rollBackConflict") : t("app.error")}
          </AlertDescription>
        </Alert>
      )}
      <Table>
        <TableBody>
          {imports.data.map((imp) => (
            <TableRow key={imp.id} className={cn(imp.rolled_back_at && "opacity-60")}>
              <TableCell>{formatDate(imp.created_at.slice(0, 10))}</TableCell>
              <TableCell>{imp.file_name || "—"}</TableCell>
              <TableCell className="text-sm text-muted-foreground whitespace-normal">
                {t("tableImport.historyCounts", {
                  written: imp.rows_written,
                  duplicate: imp.rows_duplicate,
                  unparsed: imp.rows_unparsed,
                  refused: imp.rows_refused,
                })}
              </TableCell>
              <TableCell className="text-right">
                {imp.rolled_back_at ? (
                  <span className="text-sm text-muted-foreground">
                    {t("tableImport.rolledBack", {
                      date: formatDate(imp.rolled_back_at.slice(0, 10)),
                    })}
                  </span>
                ) : (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={rollBack.isPending}
                    onClick={() => {
                      rollBack.reset();
                      setTarget(imp.id);
                    }}
                  >
                    {t("tableImport.rollBack")}
                  </Button>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <Dialog open={target !== null} onOpenChange={(open) => !open && setTarget(null)}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("tableImport.rollBack")}</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">{t("tableImport.rollBackConfirm")}</p>
          <DialogFooter>
            <Button variant="outline" onClick={() => setTarget(null)}>
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={rollBack.isPending}
              onClick={() => {
                if (target) rollBack.mutate(target, { onSettled: () => setTarget(null) });
              }}
            >
              {t("tableImport.rollBack")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
