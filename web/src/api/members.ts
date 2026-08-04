import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { apiError } from "./operations";
import type { components } from "./schema";

export type MemberInfo = components["schemas"]["MemberInfo"];
export type Role = components["schemas"]["Role"];
export type CreateMemberBody = components["schemas"]["CreateMemberRequest"];
export type UpdateMemberBody = components["schemas"]["UpdateMemberRequest"];

export function useMembers() {
  return useQuery({
    queryKey: ["members"],
    queryFn: async (): Promise<MemberInfo[]> => {
      const { data, response } = await api.GET("/api/v1/members");
      if (!data) throw new Error(`members failed: ${response.status}`);
      return data;
    },
  });
}

function useInvalidateMembers() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: ["members"] });
  };
}

export function useCreateMember() {
  const invalidate = useInvalidateMembers();
  return useMutation({
    mutationFn: async (body: CreateMemberBody) => {
      const { data, error, response } = await api.POST("/api/v1/members", { body });
      // ApiError, so the dialog can tell «логин занят» (409) from anything else
      // by the status the API contract promises rather than by the English
      // sentence it happens to carry.
      if (!data) throw apiError(response, error);
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useUpdateMemberRole() {
  const invalidate = useInvalidateMembers();
  return useMutation({
    mutationFn: async ({ userId, body }: { userId: string; body: UpdateMemberBody }) => {
      const { data, error, response } = await api.PATCH("/api/v1/members/{userId}", {
        params: { path: { userId } },
        body,
      });
      if (!data) {
        throw new Error(
          (error as { error?: string } | undefined)?.error ?? `update failed: ${response.status}`,
        );
      }
      return data;
    },
    onSuccess: invalidate,
  });
}

export function useRemoveMember() {
  const invalidate = useInvalidateMembers();
  return useMutation({
    mutationFn: async (userId: string) => {
      const { response, error } = await api.DELETE("/api/v1/members/{userId}", {
        params: { path: { userId } },
      });
      if (!response.ok) {
        throw new Error(
          (error as { error?: string } | undefined)?.error ?? `remove failed: ${response.status}`,
        );
      }
    },
    onSuccess: invalidate,
  });
}
