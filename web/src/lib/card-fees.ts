import type { CreditCardTerms } from "@/api/credit-cards";

type Fees = CreditCardTerms["fees"];

const percentOf = (amount: number, pct: string) => Math.round((amount * Number(pct)) / 100);

// cashFee is the bank's fee for taking amount out in cash with already taken
// out this period: the fixed part and the percent of what goes past the free
// part (as internal/creditcard Fees.Cash).
export function cashFee(fees: Fees, amount: number, already: number): number {
  const over = amount - Math.max(fees.cash_free_minor - already, 0);
  if (over <= 0 || (Number(fees.cash_percent) === 0 && fees.cash_fixed_minor === 0)) return 0;
  return percentOf(over, fees.cash_percent) + fees.cash_fixed_minor;
}

// transferFee is the bank's fee for moving amount off the card.
export function transferFee(fees: Fees, amount: number): number {
  if (amount <= 0 || (Number(fees.transfer_percent) === 0 && fees.transfer_fixed_minor === 0)) return 0;
  return percentOf(amount, fees.transfer_percent) + fees.transfer_fixed_minor;
}
