import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";
import type { SessionInfo } from "./session";

export type UpdateSpaceBody = components["schemas"]["UpdateSpaceRequest"];

// Updates the space's base currency (owner-only; the server also enforces
// this and answers with 403 for anyone else). The response is a fresh
// SessionInfo, so it replaces the session cache directly instead of
// triggering a refetch, and the summary is invalidated so the total
// re-converts into the new base currency.
export function useUpdateBaseCurrency() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: UpdateSpaceBody): Promise<SessionInfo> => {
      const { data, error, response } = await api.PATCH("/api/v1/space", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data) => {
      queryClient.setQueryData(["session"], data);
      void queryClient.invalidateQueries({ queryKey: ["summary"] });
    },
  });
}
