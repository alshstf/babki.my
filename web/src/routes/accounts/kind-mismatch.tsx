import { useTranslation } from "react-i18next";
import type { InstrumentType } from "@/api/instruments";
import type { OperationType } from "@/api/operations";

// Says before Save that an entry of this type cannot name a paper of this kind
// (fitsKind), as the server would refuse it. Money is exchanged, not bought.
export function KindMismatch({ type, kind }: { type: OperationType; kind: InstrumentType }) {
  const { t } = useTranslation();
  return (
    <p data-testid="operation-kind-mismatch" className="text-xs text-red-500">
      {kind === "currency"
        ? t("operationKinds.currency")
        : t("operationKinds.mismatch", {
            type: t(`operationTypes.${type}`),
            kind: t(`instrumentTypes.${kind}`),
          })}
    </p>
  );
}
