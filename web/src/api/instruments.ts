import { useEffect, useMemo } from "react";
import { keepPreviousData, useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type Instrument = components["schemas"]["Instrument"];
export type InstrumentType = components["schemas"]["InstrumentType"];
export type InstrumentsPage = components["schemas"]["InstrumentsResponse"];
export type CreateInstrumentBody = components["schemas"]["CreateInstrumentRequest"];
export type UpdateInstrumentBody = components["schemas"]["UpdateInstrumentRequest"];

// CATALOG_PAGE_SIZE is one page of the picker, well under the endpoint's
// ceiling of 200, past which the request is refused rather than trimmed.
export const CATALOG_PAGE_SIZE = 50;

// CATALOG_INDEX_PAGE_SIZE is the ceiling itself: useInstrumentIndex shows no
// page, so only the number of round trips depends on it.
const CATALOG_INDEX_PAGE_SIZE = 200;

// useInstruments reads the catalog a page at a time and keeps the pages, so
// «показать ещё» appends (#104); has_more is the server's. Always enabled: an
// empty query lists from the start.
export function useInstruments(query: string, pageSize = CATALOG_PAGE_SIZE) {
  return useInfiniteQuery({
    queryKey: ["instruments", query, pageSize],
    initialPageParam: 0,
    queryFn: async ({ pageParam }): Promise<InstrumentsPage> => {
      const { data, error, response } = await api.GET("/api/v1/instruments", {
        params: { query: { query, limit: pageSize, offset: pageParam } },
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    // The next offset is where the rows in hand end, asked only when the
    // server says more exists; counted from rows, not pages, so a short page
    // cannot skip rows.
    getNextPageParam: (lastPage, allPages) =>
      lastPage.has_more
        ? allPages.reduce((rows, page) => rows + page.instruments.length, 0)
        : undefined,
    // Keeps the previous list visible while a new query runs. Its rows then sit
    // under the new key, so `data` no longer means "answered"; the picker uses
    // isPlaceholderData for «ничего не найдено» and the offline notice
    // (instrument-picker.tsx).
    placeholderData: keepPreviousData,
  });
}

// instrumentsOf flattens the loaded pages, one rule for every reader.
export function instrumentsOf(pages?: { pages: InstrumentsPage[] }): Instrument[] {
  return pages?.pages.flatMap((page) => page.instruments) ?? [];
}

// useInstrumentIndex answers "what is this instrument called" by id, for the
// journal. It reads the whole catalog: a lookup that stopped at a page would
// print «#a1b2c3d4» for every row past it, and a broker import brings a hundred
// papers. Pages of 200, cached; the page size is in the key, so it never shares
// pages with the picker. Until the walk finishes a name may be undefined, the
// ordinary loading state.
export function useInstrumentIndex() {
  const catalog = useInstruments("", CATALOG_INDEX_PAGE_SIZE);
  const { hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage } = catalog;

  useEffect(() => {
    // hasNextPage is the server's answer. isFetchNextPageError stops the
    // walk after a failure: a failed page keeps hasNextPage true and clears
    // isFetchingNextPage, so without it the effect would retry in a tight
    // loop; react-query does the retrying. Rows already fetched keep their
    // names.
    if (hasNextPage && !isFetchingNextPage && !isFetchNextPageError) void fetchNextPage();
  }, [hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage]);

  const byId = useMemo(() => {
    const index = new Map<string, Instrument>();
    for (const instrument of instrumentsOf(catalog.data)) index.set(instrument.id, instrument);
    return index;
  }, [catalog.data]);

  return byId;
}

function useInvalidateInstruments() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: ["instruments"] });
  };
}

// useUpdateInstrument corrects a catalog row, which nothing in the interface
// could reach before: the owner's Apple and Tesla had no ISIN, the field the
// quote worker searches by, so they were never priced. networkMode "always"
// (#111).
export function useUpdateInstrument() {
  const invalidate = useInvalidateInstruments();
  const queryClient = useQueryClient();
  return useMutation({
    networkMode: "always",
    mutationFn: async ({
      id,
      body,
    }: {
      id: string;
      body: UpdateInstrumentBody;
    }): Promise<Instrument> => {
      const { data, error, response } = await api.PATCH("/api/v1/instruments/{instrumentId}", {
        params: { path: { instrumentId: id } },
        body,
      });
      if (!data) throw apiError(response, error);
      return data;
    },
    // The catalog and the positions, which carry the instrument's name,
    // ticker and frozen flag. A new valuation waits for the quote worker.
    onSuccess: () => {
      invalidate();
      void queryClient.invalidateQueries({ queryKey: ["positions"] });
      // And the paper's own page.
      void queryClient.invalidateQueries({ queryKey: ["instrument-holdings"] });
    },
  });
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

// What useAddInstrumentByISIN did: created the row or found the ISIN
// already catalogued; the caller words it.
export type AddInstrumentResult = {
  created: boolean;
  instrument: Instrument;
};

// useAddInstrumentByISIN files a paper the reconciliation found at the broker,
// from the broker's passport. It looks first: a duplicate ISIN is a 400 like any
// other bad field, and only the English body tells them apart, which a screen may
// not read. The search matches substrings of name, ticker or ISIN, so the result
// is filtered to an exact ISIN; one page suffices, since an ISIN is unique.
export function useAddInstrumentByISIN() {
  const invalidate = useInvalidateInstruments();
  return useMutation({
    // A button press waits for an answer now (#111).
    networkMode: "always",
    mutationFn: async (body: CreateInstrumentBody & { isin: string }): Promise<AddInstrumentResult> => {
      const { isin } = body;
      const found = await api.GET("/api/v1/instruments", {
        params: { query: { query: isin, limit: CATALOG_PAGE_SIZE, offset: 0 } },
      });
      if (!found.data) throw apiError(found.response, found.error);
      const existing = found.data.instruments.find(
        (candidate) => candidate.isin.toUpperCase() === isin.toUpperCase(),
      );
      if (existing) return { created: false, instrument: existing };

      const { data, error, response } = await api.POST("/api/v1/instruments", { body });
      if (!data) throw apiError(response, error);
      return { created: true, instrument: data };
    },
    onSuccess: invalidate,
  });
}
