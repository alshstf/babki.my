import type { InstrumentType } from "@/api/instruments";
import type { OperationType } from "@/api/operations";

// Which kinds of paper a hand entry of each type may name (decision Р-17): the
// server's table in internal/operation/kinds.go, one line per type, held to it
// by a Go test. A type not listed may name any.
const KINDS_FOR: Partial<Record<OperationType, readonly InstrumentType[]>> = {
  buy: ["share", "etf", "bond", "crypto", "metal", "custom"],
  sell: ["share", "etf", "bond", "crypto", "metal", "custom"],
  redemption: ["bond", "etf", "custom"],
  dividend: ["share", "etf", "custom"],
  coupon: ["bond", "custom"],
  amortization: ["bond", "custom"],
};

// fitsKind is whether an entry of type may name a paper of kind.
export function fitsKind(type: OperationType, kind: InstrumentType): boolean {
  const kinds = KINDS_FOR[type];
  return kinds === undefined || kinds.includes(kind);
}
