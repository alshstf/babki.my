import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/api/client";
import { apiError } from "@/api/operations";
import { isPositiveDecimal } from "@/lib/money";
import { EARLIEST_OPERATION_DATE, localToday } from "@/lib/dates";

// The paper a price is stated for.
export type QuotedPaper = { id: string; name: string; currency: string; bond: boolean };

// StatePriceDialog states by hand the price of a paper nobody quotes — a
// frozen fund at its net asset value, an over-the-counter estimate. It values
// every holding of the paper from that day on, until a later price from any
// source takes over.
export function StatePriceDialog({
  open,
  onOpenChange,
  paper,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  paper: QuotedPaper;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [on, setOn] = useState(localToday());
  const [price, setPrice] = useState("");
  const save = useMutation({
    mutationFn: async () => {
      const { response, error } = await api.POST("/api/v1/instruments/{instrumentId}/prices", {
        params: { path: { instrumentId: paper.id } },
        body: { on, price: price.trim().replace(",", ".") },
      });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => {
      for (const key of ["positions", "accounts", "summary", "capital", "return"]) {
        void queryClient.invalidateQueries({ queryKey: [key] });
      }
      onOpenChange(false);
    },
  });
  useEffect(() => {
    if (open) {
      setOn(localToday());
      setPrice("");
      save.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
  const valid = isPositiveDecimal(price.trim().replace(",", ".")) && on !== "";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{t("statePrice.title", { name: paper.name })}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4">
          <p className="text-sm text-muted-foreground">{t("statePrice.lead")}</p>
          <div className="grid gap-2">
            <Label htmlFor="state-price-on">{t("statePrice.on")}</Label>
            <Input
              id="state-price-on"
              type="date"
              value={on}
              min={EARLIEST_OPERATION_DATE}
              max={localToday()}
              onChange={(e) => setOn(e.target.value)}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="state-price-price">
              {paper.bond
                ? t("statePrice.pricePercent")
                : t("statePrice.price", { currency: paper.currency })}
            </Label>
            <Input
              id="state-price-price"
              inputMode="decimal"
              value={price}
              onChange={(e) => setPrice(e.target.value)}
            />
          </div>
          {save.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("statePrice.error")}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button disabled={!valid || save.isPending} onClick={() => save.mutate()}>
            {t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
