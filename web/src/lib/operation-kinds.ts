import type { InstrumentType } from "@/api/instruments";
import type { OperationType } from "@/api/operations";
// Which kinds of paper a hand entry of each type may name (decision Р-17): the
// server's own table, generated from it (api/constants.gen.ts). A type not
// listed may name any.
import { KINDS_FOR } from "@/api/constants.gen";

// fitsKind is whether an entry of type may name a paper of kind.
export function fitsKind(type: OperationType, kind: InstrumentType): boolean {
  const kinds = KINDS_FOR[type];
  return kinds === undefined || kinds.includes(kind);
}
