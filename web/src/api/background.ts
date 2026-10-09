import { useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type BackgroundTask = components["schemas"]["BackgroundTask"];

// How often the running jobs are asked about: often while something runs, so a
// bar moves; seldom otherwise, so an import started elsewhere shows up.
export const POLL_RUNNING_MS = 3_000;
export const POLL_IDLE_MS = 15_000;

// The background jobs running now (GET /api/v1/background-tasks). One query for
// the header and every account that shows them.
export function useBackgroundTasks() {
  return useQuery({
    queryKey: ["background-tasks"],
    queryFn: async (): Promise<BackgroundTask[]> => {
      const { data, error, response } = await api.GET("/api/v1/background-tasks");
      if (!data) throw apiError(response, error);
      return data;
    },
    refetchInterval: (query) => ((query.state.data?.length ?? 0) > 0 ? POLL_RUNNING_MS : POLL_IDLE_MS),
  });
}

// When a job ends, what it changed is read again: an import, a backfill or the
// registry change journals, positions and every figure made from them, and a
// screen open during the run would otherwise keep half-done numbers.
export function useRefreshWhenTasksEnd(tasks: BackgroundTask[] | undefined) {
  const queryClient = useQueryClient();
  const running = useRef<Set<number>>(new Set());
  useEffect(() => {
    if (!tasks) return;
    const now = new Set(tasks.map((task) => task.id));
    const ended = [...running.current].some((id) => !now.has(id));
    running.current = now;
    if (ended) {
      void queryClient.invalidateQueries({
        predicate: (query) => query.queryKey[0] !== "background-tasks" && query.queryKey[0] !== "session",
      });
    }
  }, [tasks, queryClient]);
}

// The stages of the broker import in their order, so its bar runs once from
// start to end instead of restarting at every stage. "settlements" (the hourly
// run only) counts with reading the operations.
const SYNC_STAGES = ["operations", "journal", "write", "registry", "reconcile"];

// taskShare is how far through a task is, 0 to 1, or null when it cannot be
// told (a stage without a count).
export function taskShare(task: BackgroundTask): number | null {
  const stage = task.total > 0 ? Math.min(task.done / task.total, 1) : null;
  if (task.kind !== "tinvest.sync") return stage;
  const at = SYNC_STAGES.indexOf(task.stage === "settlements" ? "operations" : task.stage);
  if (at < 0) return task.stage === "" ? 0 : null;
  return (at + (stage ?? 0)) / SYNC_STAGES.length;
}

// overallShare is the share of every task that can be told, averaged; null when
// none can.
export function overallShare(tasks: BackgroundTask[]): number | null {
  const shares = tasks.map(taskShare).filter((share): share is number => share !== null);
  if (shares.length === 0) return null;
  return shares.reduce((sum, share) => sum + share, 0) / shares.length;
}

// The tasks that leave an account's figures unfinished.
export function tasksOfAccount(tasks: BackgroundTask[] | undefined, accountId: string): BackgroundTask[] {
  return (tasks ?? []).filter((task) => task.account_ids.includes(accountId));
}
