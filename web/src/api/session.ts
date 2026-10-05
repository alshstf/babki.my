import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { api } from "./client";
import { apiError, ApiError } from "./operations";
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

// SignInLocked is the 429 after too many wrong passwords: nothing is compared
// until the wait is over, so the form must not blame the password. minutesLeft
// is Retry-After rounded up to minutes, or null.
export class SignInLocked extends ApiError {
  minutesLeft: number | null;

  constructor(message: string, retryAfter: string | null) {
    super(message, 429);
    const seconds = retryAfter === null ? NaN : Number(retryAfter);
    this.minutesLeft =
      Number.isFinite(seconds) && seconds > 0 ? Math.ceil(seconds / 60) : null;
  }
}

// SessionNotKept is a sign-in the server accepted and the browser did not
// keep: the cookie is HTTPS-only by default (BABKI_COOKIE_SECURE), and plain
// http drops it.
export class SessionNotKept extends Error {}

// confirmSessionKept asks who is signed in right after sign-in; only a 401
// is news about the cookie.
async function confirmSessionKept(): Promise<void> {
  let status: number;
  try {
    ({ response: { status } } = await api.GET("/api/v1/auth/me"));
  } catch {
    return;
  }
  if (status === 401) throw new SessionNotKept("the browser did not keep the session cookie");
}

// useLogin trades a username and password for a session. networkMode "always":
// the default pauses a mutation while the browser thinks it is offline, leaving
// the button disabled with nothing said and signing in later on its own.
// Failures are ApiErrors so the form tells a 401 (wrong credentials) from
// everything else by status.
export function useLogin() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    networkMode: "always",
    mutationFn: async (body: { username: string; password: string }) => {
      const { data, error, response } = await api.POST("/api/v1/auth/login", {
        body,
      });
      if (!data && response.status === 429) {
        throw new SignInLocked(
          apiError(response, error).message,
          response.headers.get("Retry-After"),
        );
      }
      if (!data) throw apiError(response, error);
      await confirmSessionKept();
      return data;
    },
    onSuccess: (data) => {
      queryClient.setQueryData(["session"], data);
      void navigate({ to: "/" });
    },
  });
}

// useSetup turns a brand-new instance into one with a space and an owner.
// networkMode "always" (#111), as for useLogin: a request held offline would leave
// the button disabled with nothing said, and set the instance up later on its own.
// The startup gate's offline notice does not help: the setup-status query has
// already answered.
export function useSetup() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    networkMode: "always",
    mutationFn: async (body: {
      space_name: string;
      username: string;
      display_name: string;
      password: string;
      setup_code?: string;
    }) => {
      const { data, error, response } = await api.POST("/api/v1/setup", {
        body,
      });
      // ApiError, so the page tells «уже настроен» (409) by status.
      if (!data) throw apiError(response, error);
      await confirmSessionKept();
      return data;
    },
    onSuccess: (data) => {
      queryClient.setQueryData(["session"], data);
      void queryClient.invalidateQueries({ queryKey: ["setup-status"] });
      void navigate({ to: "/" });
    },
  });
}

// useLogout ends the session on the server and only then clears this browser.
// Success is a bodyless 204, so it is read off the status (#88). 401 counts as
// done: there is no session left to end, as useSession reads it.
export function useLogout() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    // networkMode "always": a sign-out held offline would leave the session
    // open with nothing said, and leave for /login later on its own. The
    // browser's offline flag is a belief, wrong behind captive portals.
    networkMode: "always",
    mutationFn: async () => {
      const { error, response } = await api.POST("/api/v1/auth/logout");
      if (!response.ok && response.status !== 401) throw apiError(response, error);
    },
    // Only after the server confirms: everything cached belongs to the person
    // who left, and must not be shown to the next one.
    onSuccess: () => {
      queryClient.clear();
      void navigate({ to: "/login" });
    },
  });
}

// useChangePassword replaces the member's password; their other sessions
// end, this one continues.
export function useChangePassword() {
  return useMutation({
    mutationFn: async (body: { current_password: string; new_password: string }) => {
      const { error, response } = await api.POST("/api/v1/auth/password", { body });
      if (!response.ok) throw apiError(response, error);
    },
  });
}

// useSignOutElsewhere ends every session of the signed-in member but this one.
export function useSignOutElsewhere() {
  return useMutation({
    mutationFn: async () => {
      const { error, response } = await api.POST("/api/v1/auth/sign-out-elsewhere");
      if (!response.ok) throw apiError(response, error);
    },
  });
}
