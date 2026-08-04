import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { api } from "./client";
import { apiError } from "./operations";
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

// useLogout ends the session on the SERVER and only then clears this browser.
// Success is 204 and carries no body, so the answer has to be read off the
// status: `data` is undefined here even when everything went right, and until
// #88 this mutation looked at neither. A sign-out that failed therefore cleared
// the caches and went to the login form exactly as a successful one did, while
// the session it was supposed to destroy went on living — handleLogout
// (internal/family/http.go) only destroys it when it is actually reached.
//
// 401 is the one refusal that counts as done rather than failed: it means
// RequireAuth (internal/family/session.go) found no session it could
// authenticate — expired, unparseable, or belonging to a membership that is
// gone — so there is nothing left to sign out of, and reporting a failure would
// name an event that did not happen. useSession reads 401 on /auth/me the same
// way, for the same reason.
export function useLogout() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    mutationFn: async () => {
      const { error, response } = await api.POST("/api/v1/auth/logout");
      if (!response.ok && response.status !== 401) throw apiError(response, error);
    },
    onSuccess: () => {
      queryClient.clear();
      void navigate({ to: "/login" });
    },
  });
}
