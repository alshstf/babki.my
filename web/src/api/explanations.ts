import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type TinvestRowExplanation = components["schemas"]["TinvestRowExplanation"];
export type ExplainRowsBody = components["schemas"]["TinvestExplainRequest"];

// useExplainRows accounts for one or more broker rows with a manual journal
// operation: the named rows stop being projected and stop counting as
// unparsed, and the operation is what the journal holds for the event instead.
//
// WHY THIS EXISTS AT ALL: the broker's operation enum has no corporate actions
// of any kind, so a real event — a fund's partial redemption, a conversion —
// arrives as whatever rows carried its money, and no rule over those rows can
// see what they were. The owner can.
//
// Both lists are invalidated because both change: the unparsed list gains the
// explanation on those rows and loses them from its count, and the connection
// screen shows the sync this queues. The linked account's journal is not this
// module's to invalidate — the account screens key their own queries.
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

// useRemoveExplanation takes an explanation back. THE MANUAL OPERATION GOES
// WITH IT — that is the endpoint's whole action rather than a side effect (see
// DELETE /api/v1/tinvest/explanations/{explanationId}), and the button that
// calls this has to say so in as many words.
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
