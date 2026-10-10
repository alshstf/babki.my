import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { CategorySelect } from "@/components/category-picker";
import { rulePattern, useCreateCategoryRule, type Category } from "@/api/categories";
import { useResplitReceipt, type Receipt } from "@/api/receipts";
import { formatMinor } from "@/lib/money";

// ReceiptLine is the receipt a row's purchase is completed by: the seller and
// the number of lines, which open to the lines themselves. Someone who may
// write teaches a line's category — a rule «Позиция чека» (decision Р-36) —
// and the row is divided by the rules again.
export function ReceiptLine({ receipt, categories, canEdit }: { receipt: Receipt; categories: Category[]; canEdit: boolean }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [teaching, setTeaching] = useState<number | null>(null);
  const createRule = useCreateCategoryRule();
  const resplit = useResplitReceipt();
  const items = receipt.items;
  const seller = receipt.seller ?? t("receiptLine.noSeller");
  const teach = (name: string, categoryId: string | null) => {
    setTeaching(null);
    if (!categoryId) return;
    createRule.mutate(
      { category_id: categoryId, field: "item", pattern: rulePattern(name) },
      { onSuccess: () => resplit.mutate(receipt.id) },
    );
  };
  return (
    <div className="text-xs text-muted-foreground" data-testid="operation-receipt">
      {items.length > 0 ? (
        <button type="button" className="hover:text-foreground" aria-expanded={open} onClick={() => setOpen(!open)}>
          {t("receiptLine.summary", { seller, n: items.length })} {open ? "▴" : "▾"}
        </button>
      ) : (
        <span>{t("receiptLine.bare", { seller })}</span>
      )}
      {open && (
        <ul className="mt-0.5 grid gap-0.5 pl-2" data-testid="operation-receipt-items">
          {items.map((it, i) => (
            <li key={i} className="grid gap-0.5">
              <div className="flex justify-between gap-3">
                <span className="min-w-0 whitespace-normal">
                  {it.name}
                  {it.quantity !== "1" && ` × ${it.quantity}`}
                  {canEdit && teaching !== i && (
                    <button type="button" className="ml-2 underline-offset-2 hover:underline" onClick={() => setTeaching(i)}>
                      {t("receiptLine.teach")}
                    </button>
                  )}
                </span>
                <span className="tabular-nums">{formatMinor(it.sum_minor, "RUB")}</span>
              </div>
              {teaching === i && (
                <div className="max-w-64" data-testid="receipt-teach">
                  <CategorySelect categories={categories} kind="expense" value={null} onChange={(id) => teach(it.name, id)} />
                  <p>{t("receiptLine.teachHint", { pattern: rulePattern(it.name) })}</p>
                </div>
              )}
            </li>
          ))}
          {canEdit && (
            <li>
              <Button type="button" size="sm" variant="ghost" className="h-6 px-1 text-xs" disabled={resplit.isPending} onClick={() => resplit.mutate(receipt.id)}>
                {t("receiptLine.resplit")}
              </Button>
            </li>
          )}
        </ul>
      )}
    </div>
  );
}
