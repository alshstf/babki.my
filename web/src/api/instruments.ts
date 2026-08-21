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

// CATALOG_PAGE_SIZE is how many instruments one page of the picker's list
// holds. Comfortably under the endpoint's own ceiling (200), which matters more
// here than it does for the journal: past that ceiling the request is REFUSED
// rather than quietly cut down, so a page size chosen carelessly would not be a
// wasted round trip but a search box that answers nothing at all.
export const CATALOG_PAGE_SIZE = 50;

// CATALOG_INDEX_PAGE_SIZE is what useInstrumentIndex asks for, and it is the
// ceiling itself: nothing there shows a page to anybody, so the only thing the
// size decides is how many round trips the whole catalog costs.
const CATALOG_INDEX_PAGE_SIZE = 200;

// useInstruments reads the catalog one page at a time and keeps the pages it
// has read, so "показать ещё" appends rather than refetches.
//
// It used to be a single query for the first fifty rows and nothing else. The
// endpoint took no `offset` at all, so there was no second page to ask for even
// if the hook had wanted one: the fifty-first instrument was reachable only by
// typing enough of a name to bring it into the first fifty, and only by someone
// who already knew the name to type (#104). Both halves of that are gone —
// `offset` continues the listing, and whether to offer the control is
// `has_more`, which only the server can answer.
//
// Always fetches (no `enabled` gate): an empty query lists the catalog from the
// start, which is what a picker wants before the user has typed anything.
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
    // The next offset is where the rows already in hand end, and it is asked
    // for only when the server says something is there. Counted from the rows
    // received rather than multiplied out of the page count, the same as the
    // journal's: the two agree only while every page comes back full, and a
    // page shorter than asked for is exactly where multiplying would skip rows.
    getNextPageParam: (lastPage, allPages) =>
      lastPage.has_more
        ? allPages.reduce((rows, page) => rows + page.instruments.length, 0)
        : undefined,
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

// instrumentsOf flattens the pages a paged catalog query has loaded so far.
// One statement rather than one per reader, because "the rows in hand" is a
// question two screens ask and neither should answer differently.
export function instrumentsOf(pages?: { pages: InstrumentsPage[] }): Instrument[] {
  return pages?.pages.flatMap((page) => page.instruments) ?? [];
}

// useInstrumentIndex answers "what is this instrument called", by id, for a
// screen that holds instrument ids and shows names — the journal, today.
//
// IT READS THE WHOLE CATALOG, page after page, and that is the difference
// between a lookup and a listing. A listing may honestly stop where the reader
// stopped scrolling; a lookup that stops there prints «#a1b2c3d4» in place of a
// name for every row past the cut, with nothing on the screen saying that a
// name exists and simply was not fetched. That was tolerable while the catalog
// was a handful of instruments typed in by hand and is not now: one broker
// import brings in a hundred papers and more arrive with every sync.
//
// The cost is bounded and small: pages of 200, so a catalog of a few hundred is
// one or two requests, cached for as long as react-query holds the key. The
// page size is part of that key, so this walk and the picker's own paging of
// the same catalog never share a cache entry — one cannot leave the other with
// pages it did not ask for, or with «показать ещё» already spent.
//
// While the walk is still going the map is incomplete, and a caller gets
// `undefined` for a name that is on its way. That is the ordinary loading state
// every reader here already has to handle, not a new kind of silence.
export function useInstrumentIndex() {
  const catalog = useInstruments("", CATALOG_INDEX_PAGE_SIZE);
  const { hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage } = catalog;

  useEffect(() => {
    // `hasNextPage` is the server's answer, not a count of anything here, so
    // the walk stops where the catalog ends and not where a page happens to
    // look short.
    //
    // isFetchNextPageError IS LOAD-BEARING AND NOT BELT-AND-BRACES. A page that
    // fails does NOT clear hasNextPage — the pages already in hand still say
    // there is more behind them — and it does clear isFetchingNextPage, so
    // without this term the effect asks again the instant the failure lands,
    // for ever: a tight loop against a server that is already answering badly,
    // from a screen nobody has to touch. react-query does the retrying (with
    // backoff, and it gives up), and when it has given up this stays out of the
    // way. The failure is not silent: the rows already fetched keep their
    // names, and a row whose instrument never arrived shows the id fallback
    // this hook has always had for a catalog still loading.
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

// useUpdateInstrument corrects a row of the catalog.
//
// IT EXISTS BECAUSE A PAPER ENTERED BY HAND COULD NOT BE CORRECTED AT ALL. The
// endpoint has always been there; nothing in the interface reached it, so an
// instrument created through a trade dialog — with whatever was typed into it —
// stayed that way for ever. The owner's Apple and Tesla have no ISIN, which is
// the field the quote worker searches by, so neither has ever been priced, and
// their positions go into the account's total counted at nought.
//
// `networkMode: "always"` for the reason every mutation the user pressed a
// button for carries it (#111): offline, react-query would otherwise hold the
// call in a pending state that looks exactly like a slow server, and the form
// would hang instead of saying what happened.
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
    // The catalog AND the positions computed from it. A position row carries
    // the instrument's own fields — its name, its ticker, whether it is frozen
    // — so a correction that did not reach that query would leave the old
    // spelling on screen beside the new one until something else refetched.
    // (The VALUATION does not appear on the next render either way: the quote
    // worker has to run and find the paper first, which is a job and not a
    // request.)
    onSuccess: () => {
      invalidate();
      void queryClient.invalidateQueries({ queryKey: ["positions"] });
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
