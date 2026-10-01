import { useTranslation } from "react-i18next";
import { X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { EARLIEST_OPERATION_DATE } from "@/lib/dates";
import { isPositiveDecimal, parseToMinor } from "@/lib/money";
import { normalizeQuantity, quantitiesMatch, quantitySum } from "@/lib/quantity";
import type { StatedPurchase } from "@/api/arrivals";

// One purchase as the owner types it. Price per share or the total — one of
// the two; the server strikes the cost from a price, so the screen never
// multiplies money (see StatedPurchase in the API contract).
export type PurchaseRow = {
  id: number;
  quantity: string;
  price: string;
  total: string;
  fee: string;
  acquiredOn: string;
};

let nextRowId = 1;

export function newPurchaseRow(quantity = ""): PurchaseRow {
  return { id: nextRowId++, quantity, price: "", total: "", fee: "", acquiredOn: "" };
}

const PRICE_RE = /^\d+(\.\d{1,10})?$/;

export type RowProblem = "quantity" | "priceOrTotal" | "price" | "total" | "fee" | "date";

// rowProblem names the first thing that keeps a row from being sent, or null.
// arrivedOn is the day the shares arrived: they were bought before it.
export function rowProblem(row: PurchaseRow, arrivedOn: string): RowProblem | null {
  if (!isPositiveDecimal(normalizeQuantity(row.quantity))) return "quantity";
  const hasPrice = row.price.trim() !== "";
  const hasTotal = row.total.trim() !== "";
  if (hasPrice === hasTotal) return "priceOrTotal";
  if (hasPrice && !PRICE_RE.test(normalizeQuantity(row.price))) return "price";
  if (hasTotal) {
    const total = parseToMinor(row.total);
    if (total === null || total < 0) return "total";
  }
  if (row.fee.trim() !== "") {
    const fee = parseToMinor(row.fee);
    if (fee === null || fee < 0) return "fee";
  }
  if (row.acquiredOn !== "" && (row.acquiredOn > arrivedOn || row.acquiredOn < EARLIEST_OPERATION_DATE)) {
    return "date";
  }
  return null;
}

// toStatedPurchase is a row as the wire carries it. Call it only on a row
// rowProblem has nothing to say about.
export function toStatedPurchase(row: PurchaseRow): StatedPurchase {
  const hasPrice = row.price.trim() !== "";
  return {
    quantity: normalizeQuantity(row.quantity),
    price: hasPrice ? normalizeQuantity(row.price) : null,
    cost_minor: hasPrice ? null : parseToMinor(row.total),
    fee_minor: row.fee.trim() === "" ? 0 : (parseToMinor(row.fee) ?? 0),
    acquired_on: row.acquiredOn === "" ? null : row.acquiredOn,
  };
}

// purchasesReady says whether the rows can be sent as the purchases behind
// `expected` shares: every row is sound and together they are exactly those
// shares.
export function purchasesReady(rows: PurchaseRow[], arrivedOn: string, expected: string): boolean {
  return (
    rows.length > 0 &&
    rows.every((row) => rowProblem(row, arrivedOn) === null) &&
    quantitiesMatch(
      rows.map((row) => normalizeQuantity(row.quantity)),
      expected,
    )
  );
}

export function PurchasesEditor({
  rows,
  onChange,
  currency,
  arrivedOn,
  expected,
  idPrefix,
}: {
  rows: PurchaseRow[];
  onChange: (rows: PurchaseRow[]) => void;
  currency: string;
  arrivedOn: string;
  // How many shares the purchases must account for, as the wire writes it.
  expected: string;
  idPrefix: string;
}) {
  const { t } = useTranslation();
  const update = (id: number, patch: Partial<PurchaseRow>) =>
    onChange(rows.map((row) => (row.id === id ? { ...row, ...patch } : row)));
  const quantities = rows.map((row) => normalizeQuantity(row.quantity));
  const counted = quantitySum(quantities.filter((q) => isPositiveDecimal(q)));
  const matched = quantitiesMatch(quantities, expected);

  return (
    <div className="grid gap-2" data-testid={`${idPrefix}-editor`}>
      {rows.map((row, index) => {
        const touched = row.quantity !== "" || row.price !== "" || row.total !== "" || row.fee !== "" || row.acquiredOn !== "";
        const problem = touched ? rowProblem(row, arrivedOn) : null;
        const field = (name: string) => `${idPrefix}-${index}-${name}`;
        return (
          <div key={row.id} className="grid gap-1 rounded-md border p-2">
            <div className="grid grid-cols-2 gap-2 sm:grid-cols-[1fr_1fr_1fr_1fr_1.4fr_auto] sm:items-end">
              <label className="grid gap-1 text-xs text-muted-foreground" htmlFor={field("quantity")}>
                {t("purchases.quantity")}
                <Input
                  id={field("quantity")}
                  inputMode="decimal"
                  value={row.quantity}
                  onChange={(e) => update(row.id, { quantity: e.target.value })}
                />
              </label>
              <label className="grid gap-1 text-xs text-muted-foreground" htmlFor={field("price")}>
                {t("purchases.price", { currency })}
                <Input
                  id={field("price")}
                  inputMode="decimal"
                  value={row.price}
                  onChange={(e) => update(row.id, { price: e.target.value })}
                />
              </label>
              <label className="grid gap-1 text-xs text-muted-foreground" htmlFor={field("total")}>
                {t("purchases.total", { currency })}
                <Input
                  id={field("total")}
                  inputMode="decimal"
                  value={row.total}
                  onChange={(e) => update(row.id, { total: e.target.value })}
                />
              </label>
              <label className="grid gap-1 text-xs text-muted-foreground" htmlFor={field("fee")}>
                {t("purchases.fee", { currency })}
                <Input
                  id={field("fee")}
                  inputMode="decimal"
                  value={row.fee}
                  onChange={(e) => update(row.id, { fee: e.target.value })}
                />
              </label>
              <label className="grid gap-1 text-xs text-muted-foreground" htmlFor={field("date")}>
                {t("purchases.date")}
                <Input
                  id={field("date")}
                  type="date"
                  min={EARLIEST_OPERATION_DATE}
                  max={arrivedOn}
                  value={row.acquiredOn}
                  onChange={(e) => update(row.id, { acquiredOn: e.target.value })}
                />
              </label>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label={t("purchases.removeRow")}
                disabled={rows.length === 1}
                onClick={() => onChange(rows.filter((r) => r.id !== row.id))}
              >
                <X className="size-4" />
              </Button>
            </div>
            {problem && (
              <p className="text-xs text-red-500" data-testid={`${idPrefix}-problem`}>
                {problem === "quantity" && t("purchases.problemQuantity")}
                {problem === "priceOrTotal" && t("purchases.problemPriceOrTotal")}
                {problem === "price" && t("purchases.problemPrice")}
                {problem === "total" && t("purchases.problemTotal")}
                {problem === "fee" && t("purchases.problemFee")}
                {problem === "date" && t("purchases.problemDate")}
              </p>
            )}
          </div>
        );
      })}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Button type="button" variant="outline" size="sm" onClick={() => onChange([...rows, newPurchaseRow()])}>
          {t("purchases.addRow")}
        </Button>
        <span
          data-testid={`${idPrefix}-counted`}
          className={matched ? "text-xs text-muted-foreground" : "text-xs text-amber-600"}
        >
          {t("purchases.counted", { counted: counted ?? "0", expected })}
        </span>
      </div>
    </div>
  );
}
