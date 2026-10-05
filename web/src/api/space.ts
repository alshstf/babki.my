import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";
import type { SessionInfo } from "./session";

export type UpdateSpaceBody = components["schemas"]["UpdateSpaceRequest"];

// Updates the space's base currency and/or the owner's tax residency
// (owner-only, enforced by the server too). The response is a fresh SessionInfo
// and replaces the session cache. A new base currency invalidates everything
// converted into the old one (summary, balances, journal, positions), or cached
// figures would show under the new sign. A new country changes no figure but what
// the positions response says about them (cost_basis_rules), so positions are
// invalidated.
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
