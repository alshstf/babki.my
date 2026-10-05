import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type TinvestRowExplanation = components["schemas"]["TinvestRowExplanation"];
export type ExplainRowsBody = components["schemas"]["TinvestExplainRequest"];

// useExplainRows accounts for broker rows with one manual journal operation:
// the rows stop being projected and counted as unparsed. The broker sends no
// corporate actions, so the owner says what the rows were. Invalidates the
// unparsed list and the connection (which shows the queued sync); the account
// screens key their own queries.
export function useExplainRows(connectionId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    networkMode: "always",
    mutationFn: async (vars: { linkId: string; body: ExplainRowsBody }) => {
      const { data, error, response } = await api.POST(
        "/api/v1/tinvest/links/{linkId}/explanations",
        { params: { path: { linkId: vars.linkId } }, body: vars.body },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["tinvest-unparsed", connectionId] });
      void queryClient.invalidateQueries({ queryKey: ["tinvest-connections"] });
    },
  });
}

// useRemoveExplanation takes an explanation back, and its manual operation
// goes with it; the button must say so.
export function useRemoveExplanation(connectionId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    networkMode: "always",
    mutationFn: async (explanationId: string) => {
      const { data, error, response } = await api.DELETE(
        "/api/v1/tinvest/explanations/{explanationId}",
        { params: { path: { explanationId } } },
      );
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["tinvest-unparsed", connectionId] });
      void queryClient.invalidateQueries({ queryKey: ["tinvest-connections"] });
    },
  });
}
