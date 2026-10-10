import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type RecurringPayment = components["schemas"]["RecurringPayment"];

// The regular payments the journals show (GET /api/v1/recurring).
export function useRecurring() {
  return useQuery({
    queryKey: ["recurring"],
    queryFn: async (): Promise<RecurringPayment[]> => {
      const { data, error, response } = await api.GET("/api/v1/recurring");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

export type RecurringKey = components["schemas"]["RecurringKey"];

export function keyOf(p: RecurringPayment): RecurringKey {
  return { name: p.name, incoming: p.amount_minor > 0, currency: p.currency };
}

// Hides a payment the family says is not regular (hide true) or takes it
// back; the list and the forecast are read again.
export function useHideRecurring() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ key, hide }: { key: RecurringKey; hide: boolean }) => {
      const { error, response } = hide
        ? await api.PUT("/api/v1/recurring/hidden", { body: key })
        : await api.DELETE("/api/v1/recurring/hidden", { body: key });
      if (!response.ok) throw apiError(response, error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["recurring"] });
      void queryClient.invalidateQueries({ queryKey: ["forecast"] });
    },
  });
}
