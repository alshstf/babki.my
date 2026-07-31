import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";
import type { SessionInfo } from "./session";

export type UpdateSpaceBody = components["schemas"]["UpdateSpaceRequest"];

// Updates the space's base currency (owner-only; the server also enforces
// this and answers with 403 for anyone else). The response is a fresh
// SessionInfo, so it replaces the session cache directly instead of
// triggering a refetch. Every cache holding a figure converted into the old
// base currency is invalidated so it's refetched instead of being
// relabelled with the new currency while still holding the old one's
// arithmetic: the summary total, each account's balance_in_base, the
// operations journal's in_base column (account/detail.tsx reads
// baseCurrency from this same session, so a cached journal row would render
// under the new currency's symbol the instant setQueryData above lands) and
// each account's positions.
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
      void queryClient.invalidateQueries({ queryKey: ["accounts"] });
      void queryClient.invalidateQueries({ queryKey: ["operations"] });
      void queryClient.invalidateQueries({ queryKey: ["positions"] });
    },
  });
}
