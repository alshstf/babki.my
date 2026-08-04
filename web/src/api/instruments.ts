import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Instrument = components["schemas"]["Instrument"];
export type InstrumentType = components["schemas"]["InstrumentType"];
export type CreateInstrumentBody = components["schemas"]["CreateInstrumentRequest"];

// Always fetches (no `enabled` gate): an empty query returns the first page
// of the full instrument list, capped at 50, which is what a picker wants
// before the user has typed anything.
export function useInstruments(query: string) {
  return useQuery({
    queryKey: ["instruments", query],
    queryFn: async (): Promise<Instrument[]> => {
      const { data, error, response } = await api.GET("/api/v1/instruments", {
        params: { query: { query, limit: 50 } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    // Keep the previous result list visible while a new query (e.g. each
    // keystroke in the picker's search box) is in flight, instead of
    // collapsing to a loading state and flashing the list away.
    //
    // What that means for whoever reads the result: the rows arrive under the
    // NEW query key while belonging to the previous one, so `data !== undefined`
    // no longer means "this query has answered". react-query flags them with
    // isPlaceholderData, and the picker decides its two verdicts — «ничего не
    // найдено» and the offline notice — off that flag rather than off `data`,
    // because both verdicts are about the query in the box now
    // (instrument-picker.tsx).
    placeholderData: keepPreviousData,
  });
}

function useInvalidateInstruments() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: ["instruments"] });
  };
}

export function useCreateInstrument() {
  const invalidate = useInvalidateInstruments();
  return useMutation({
    mutationFn: async (body: CreateInstrumentBody): Promise<Instrument> => {
      const { data, error, response } = await api.POST("/api/v1/instruments", { body });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}
