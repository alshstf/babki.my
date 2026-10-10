import type { AccountWithBalance } from "@/api/accounts";

// isDebt is an account whose money below zero is what it owes, not a
// shortfall: a credit card or a loan (the server's LiabilityTypes).
export function isDebt(account: Pick<AccountWithBalance, "type">): boolean {
  return account.type === "credit_card" || account.type === "loan";
}
