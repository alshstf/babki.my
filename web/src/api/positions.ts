import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Position = components["schemas"]["Position"];

export function usePositions(accountId: string, enabled = true) {
  return useQuery({
    queryKey: ["positions", accountId],
    queryFn: async (): Promise<Position[]> => {
      const { data, error, response } = await api.GET("/api/v1/accounts/{accountId}/positions", {
        params: { path: { accountId } },
      });
      if (!data) throw apiError(response, error);
      return data.positions;
    },
    enabled,
  });
}
