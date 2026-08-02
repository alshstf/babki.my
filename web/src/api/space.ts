import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";
import type { SessionInfo } from "./session";

export type UpdateSpaceBody = components["schemas"]["UpdateSpaceRequest"];

// Updates the space's settings — base currency and/or the owner's country of
// tax residency (owner-only; the server also enforces this and answers with
// 403 for anyone else). The response is a fresh SessionInfo, so it replaces
// the session cache directly instead of triggering a refetch.
//
// Both fields invalidate the positions cache, for different reasons.
//
// The base currency: every cache holding a figure converted into the OLD base
// currency has to be refetched instead of being relabelled with the new
// currency while still holding the old one's arithmetic — the summary total,
// each account's balance_in_base, the operations journal's in_base column
// (account/detail.tsx reads baseCurrency from this same session, so a cached
// journal row would render under the new currency's symbol the instant
// setQueryData above lands) and each account's positions.
//
// The country: it changes no figure at all, but it changes what the positions
// response SAYS about them — cost_basis_rules travels with those numbers and
// is what the screen shows next to them. A stale cache would leave the old
// country's statement standing over the new country's figures, which is
// exactly the mismatch this whole mechanism exists to prevent. Only the
// positions cache carries that statement, so only it is invalidated when the
// currency did not also change.
export function useUpdateSpace() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: UpdateSpaceBody): Promise<SessionInfo> => {
      const { data, error, response } = await api.PATCH("/api/v1/space", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: (data, body) => {
      queryClient.setQueryData(["session"], data);
      void queryClient.invalidateQueries({ queryKey: ["positions"] });
      if (body.base_currency !== undefined) {
        void queryClient.invalidateQueries({ queryKey: ["summary"] });
        void queryClient.invalidateQueries({ queryKey: ["accounts"] });
        void queryClient.invalidateQueries({ queryKey: ["operations"] });
      }
    },
  });
}
