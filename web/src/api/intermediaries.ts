import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Intermediary = components["schemas"]["Intermediary"];

// The program's catalog of banks and brokers (GET /api/v1/intermediaries,
// decision Р-47); it changes only with a release.
export function useIntermediaries() {
  return useQuery({
    queryKey: ["intermediaries"],
    staleTime: Infinity,
    queryFn: async (): Promise<Intermediary[]> => {
      const { data, error, response } = await api.GET("/api/v1/intermediaries");
      if (!data) throw apiError(response, error);
      return data.intermediaries;
    },
  });
}
