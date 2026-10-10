import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError, useInvalidateJournal } from "./operations";
import type { components } from "./schema";

export type Loan = components["schemas"]["Loan"];
export type LoanTerms = components["schemas"]["LoanTerms"];
export type LoanRow = components["schemas"]["LoanRow"];
export type LoanPaymentBody = components["schemas"]["LoanPaymentRequest"];

// A loan account's terms and schedule; null until terms are stated.
export function useLoan(accountId: string, enabled: boolean) {
  return useQuery({
    queryKey: ["loan", accountId],
    enabled,
    queryFn: async (): Promise<Loan | null> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/loan", {
        params: { path: { accountId } },
      });
      if (response.status === 404) return null;
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

export function useSetLoanTerms(accountId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: LoanTerms): Promise<Loan> => {
      const { data, error, response } = await api.PUT("/api/v1/accounts/{accountId}/loan", {
        params: { path: { accountId } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["loan", accountId] }),
  });
}

// Records a payment: the interest as a spending from the paying account, the
// rest a transfer to the loan.
export function useRecordLoanPayment(accountId: string) {
  const invalidate = useInvalidateJournal();
  return useMutation({
    mutationFn: async (body: LoanPaymentBody): Promise<void> => {
      const { error, response } = await api.POST("/api/v1/accounts/{accountId}/loan/payments", {
        params: { path: { accountId } },
        body,
      });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: (_, body) => invalidate([accountId, body.from_account_id]),
  });
}
