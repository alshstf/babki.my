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

// useLogin trades a username and a password for a session.
//
// It runs with networkMode "always" for the same reason useLogout does, and
// against the same defect: react-query's default, networkMode "online", PAUSES
// a mutation while the browser reports itself offline. Nothing is sent, nothing
// fails, isError stays false — so the form's alert has nothing to show — and
// isPending stays true, which disables the button that would try again. The
// reader presses «Войти», sees nothing happen and nothing said, and cannot tell
// a dead connection from a slow one; then, whenever the connection returns, the
// held request goes out and the app signs in on its own, long after anyone
// asked. Signing in is worth only what it is worth AT THE MOMENT it is asked
// for, so it is attempted whatever the browser believes about the network — a
// belief that is wrong behind a captive portal and wrong again on a connection
// that is up but going nowhere — and a dead connection becomes an ordinary
// failure the screen already knows how to report.
//
// The failure is raised as an ApiError so the form can tell the ONE refusal
// this endpoint declares (401 — the credentials were wrong) from everything
// else, by status rather than by the sentence the server happened to write. It
// used to raise a bare Error and the form captioned every failure «Неверный
// логин или пароль», which for a dead connection or a broken server sends the
// reader to change a password that was never wrong.
// SignInLocked is the 429 the sign-in door answers once too many wrong
// passwords have been tried: nothing is compared until the wait is over, so the
// right password is refused too, and the form has to say that rather than blame
// the password. minutesLeft is the server's own Retry-After rounded up to whole
// minutes, or null when the header did not arrive.
export class SignInLocked extends ApiError {
  minutesLeft: number | null;

  constructor(message: string, retryAfter: string | null) {
    super(message, 429);
    const seconds = retryAfter === null ? NaN : Number(retryAfter);
    this.minutesLeft =
      Number.isFinite(seconds) && seconds > 0 ? Math.ceil(seconds / 60) : null;
  }
}

// SessionNotKept is a sign-in the server accepted and the browser did not keep.
// The session cookie is HTTPS-only by default (BABKI_COOKIE_SECURE), and a page
// opened over plain http gets one the browser will not store: the next request
// arrives signed out, and without this the form would simply come back with
// nothing said.
export class SessionNotKept extends Error {}

// confirmSessionKept asks the server who is signed in, right after it said
// someone is. Only a 401 is news: anything else — a slow or dropped answer —
// says nothing about the cookie, and the sign-in that did succeed goes on.
async function confirmSessionKept(): Promise<void> {
  let status: number;
  try {
    ({ response: { status } } = await api.GET("/api/v1/auth/me"));
  } catch {
    return;
  }
  if (status === 401) throw new SessionNotKept("the browser did not keep the session cookie");
}

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
//
// It runs with networkMode "always" for the reason useLogin and useLogout do,
// and against the same defect — the one screen that had been left out of it
// (#111). react-query's default, networkMode "online", PAUSES a mutation while
// the browser reports itself offline: nothing is sent, nothing fails, isError
// stays false — so the alert below has nothing to show — and isPending stays
// true, which disables the button that would try again. The reader presses
// «Создать», sees nothing happen and nothing said, and cannot tell a dead
// connection from a slow one. Then, whenever the connection returns, the held
// request goes out and the instance sets itself up on its own, an arbitrary
// time after anyone asked — with a space name, a login and a password that
// were typed for a decision the reader may since have walked away from.
//
// Setting an instance up is worth only what it is worth AT THE MOMENT it is
// asked for, so it is attempted whatever the browser believes about the
// network — a belief that is wrong behind a captive portal and wrong again on
// a connection that is up but going nowhere — and a dead connection becomes an
// ordinary failure the page already knows how to report.
//
// The startup gate does read a PAUSED state and say so (see Gate in
// router.tsx, app.startupOffline), and it is no help here: it gates on the
// setup-status QUERY, which has already answered by the time this form is on
// screen. Nothing watches the connection afterwards.
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
      // ApiError, so the page can tell «уже настроен» (409) from anything else
      // by the status the API contract promises rather than by the English
      // sentence it happens to carry.
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
    // The one mutation here that may not be queued. react-query's default,
    // networkMode "online", PAUSES a mutation while the browser reports itself
    // offline: nothing is sent, nothing fails, isError stays false — so the
    // banner in AppLayout has nothing to show — and isPending stays true, which
    // disables the button that would try again. The reader clicks «Выйти», sees
    // nothing happen and nothing said, and walks away from a session that is
    // still open. Then, whenever the connection returns, the held request goes
    // out and the app leaves for /login on its own, long after anyone asked.
    //
    // Signing out is worth only what it is worth AT THE MOMENT it is asked for,
    // so it is attempted whatever the browser believes about the network, and a
    // dead connection becomes an ordinary failure the screen already knows how
    // to report. That the browser's own flag is a belief and not a fact — it is
    // wrong behind a captive portal, and wrong again on a connection that is up
    // but going nowhere — is the second reason to let the request decide.
    networkMode: "always",
    mutationFn: async () => {
      const { error, response } = await api.POST("/api/v1/auth/logout");
      if (!response.ok && response.status !== 401) throw apiError(response, error);
    },
    // Only after the server confirms. Everything this browser holds belongs to
    // the person who just left: accounts, family, positions. Without the clear
    // they stay in the cache, and the next person at a shared computer is shown
    // them — rendered from memory, before any refetch can come back 401.
    onSuccess: () => {
      queryClient.clear();
      void navigate({ to: "/login" });
    },
  });
}

// useChangePassword replaces the signed-in member's password; every other
// session of theirs ends, this one goes on.
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
