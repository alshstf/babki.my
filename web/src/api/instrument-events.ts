import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type InstrumentEvent = components["schemas"]["InstrumentEvent"];
export type InstrumentEventKind = components["schemas"]["InstrumentEventKind"];
export type CreateInstrumentEventBody =
  components["schemas"]["CreateInstrumentEventRequest"];
export type InstrumentEventWritten =
  components["schemas"]["InstrumentEventWritten"];

// useInstrumentEvents reads the whole corporate-actions registry.
//
// NOT PAGED, because the endpoint is not: it is one row per corporate action of
// every paper anyone here holds, plus whatever the exchange published — 37 rows
// on the live instance. If that ever stops being small the endpoint gains an
// envelope and this gains the paging to match; until then, asking for pages
// would be machinery for a problem nobody has.
export function useInstrumentEvents() {
  return useQuery({
    queryKey: ["instrument-events"],
    queryFn: async (): Promise<InstrumentEvent[]> => {
      const { data, error, response } = await api.GET("/api/v1/instrument-events");
      if (!data) throw apiError(response, error);
      return data.events;
    },
  });
}

// Everything a recorded or removed event can change: the registry itself, and
// the journals it was carried into — positions are computed from those, and the
// account screens read them.
function useInvalidateRegistry() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: ["instrument-events"] });
    void queryClient.invalidateQueries({ queryKey: ["positions"] });
    void queryClient.invalidateQueries({ queryKey: ["operations"] });
  };
}

export function useCreateInstrumentEvent() {
  const invalidate = useInvalidateRegistry();
  return useMutation({
    // The owner pressed a button and is waiting for an answer now; offline,
    // react-query would otherwise park the call in a pending state that looks
    // exactly like a slow server (#111).
    networkMode: "always",
    mutationFn: async (
      body: CreateInstrumentEventBody,
    ): Promise<InstrumentEventWritten> => {
      const { data, error, response } = await api.POST("/api/v1/instrument-events", {
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useDeleteInstrumentEvent() {
  const invalidate = useInvalidateRegistry();
  return useMutation({
    networkMode: "always",
    mutationFn: async (id: string): Promise<InstrumentEventWritten> => {
      const { data, error, response } = await api.DELETE(
        "/api/v1/instrument-events/{eventId}",
        { params: { path: { eventId: id } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}
