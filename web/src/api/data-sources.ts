import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type DataSource = components["schemas"]["DataSource"];

// How the background jobs that fetch outside data last ended (see GET
// /api/v1/data-sources).
export function useDataSources() {
  return useQuery({
    queryKey: ["data-sources"],
    queryFn: async (): Promise<DataSource[]> => {
      const { data, error, response } = await api.GET("/api/v1/data-sources");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}
