import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type TelegramStatus = components["schemas"]["TelegramStatus"];
export type TelegramLink = components["schemas"]["TelegramLink"];

// The space's Telegram bot and the member's chat (GET /api/v1/telegram,
// decision Р-61 Б). While a code waits to be sent to the bot, asked again
// every few seconds, so the screen sees the chat tied.
export function useTelegram(waiting = false) {
  return useQuery({
    queryKey: ["telegram"],
    queryFn: async (): Promise<TelegramStatus> => {
      const { data, error, response } = await api.GET("/api/v1/telegram");
      if (!data) throw apiError(response, error);
      return data;
    },
    refetchInterval: waiting ? 5000 : false,
  });
}

function useKeep() {
  const queryClient = useQueryClient();
  return (data: TelegramStatus) => queryClient.setQueryData(["telegram"], data);
}

export function useSetTelegramBot() {
  const keep = useKeep();
  return useMutation({
    mutationFn: async (token: string): Promise<TelegramStatus> => {
      const { data, error, response } = await api.PUT("/api/v1/telegram/bot", { body: { token } });
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: keep,
  });
}

export function useDeleteTelegramBot() {
  const keep = useKeep();
  return useMutation({
    mutationFn: async (): Promise<TelegramStatus> => {
      const { data, error, response } = await api.DELETE("/api/v1/telegram/bot");
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: keep,
  });
}

export function useLinkTelegram() {
  return useMutation({
    mutationFn: async (): Promise<TelegramLink> => {
      const { data, error, response } = await api.POST("/api/v1/telegram/link");
      if (!data) throw apiError(response, error);
      return data;
    },
  });
}

export function useUnlinkTelegram() {
  const keep = useKeep();
  return useMutation({
    mutationFn: async (): Promise<TelegramStatus> => {
      const { data, error, response } = await api.DELETE("/api/v1/telegram/link");
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: keep,
  });
}
