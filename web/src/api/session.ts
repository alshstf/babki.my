import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { api } from "./client";
import type { components } from "./schema";

export type SessionInfo = components["schemas"]["SessionInfo"];

// useSession returns null data when unauthenticated (401 is not an error here).
export function useSession() {
  return useQuery({
    queryKey: ["session"],
    queryFn: async (): Promise<SessionInfo | null> => {
      const { data, response } = await api.GET("/api/v1/auth/me");
      if (response.status === 401) return null;
      if (!data) throw new Error(`me failed: ${response.status}`);
      return data;
    },
  });
}

export function useSetupStatus() {
  return useQuery({
    queryKey: ["setup-status"],
    queryFn: async () => {
      const { data } = await api.GET("/api/v1/setup/status");
      if (!data) throw new Error("setup status failed");
      return data;
    },
  });
}

export function useLogin() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    mutationFn: async (body: { username: string; password: string }) => {
      const { data, error, response } = await api.POST("/api/v1/auth/login", {
        body,
      });
      if (!data) {
        throw new Error(
          (error as { error?: string } | undefined)?.error ??
            `login failed: ${response.status}`,
        );
      }
      return data;
    },
    onSuccess: (data) => {
      queryClient.setQueryData(["session"], data);
      void navigate({ to: "/" });
    },
  });
}

export function useSetup() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    mutationFn: async (body: {
      space_name: string;
      username: string;
      display_name: string;
      password: string;
    }) => {
      const { data, error, response } = await api.POST("/api/v1/setup", {
        body,
      });
      if (!data) {
        throw new Error(
          (error as { error?: string } | undefined)?.error ??
            `setup failed: ${response.status}`,
        );
      }
      return data;
    },
    onSuccess: (data) => {
      queryClient.setQueryData(["session"], data);
      void queryClient.invalidateQueries({ queryKey: ["setup-status"] });
      void navigate({ to: "/" });
    },
  });
}

export function useLogout() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    mutationFn: async () => {
      await api.POST("/api/v1/auth/logout");
    },
    onSuccess: () => {
      queryClient.clear();
      void navigate({ to: "/login" });
    },
  });
}
