import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Trash2 } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useSession } from "@/api/session";
import {
  useMembers,
  useRemoveMember,
  useUpdateMemberRole,
  type MemberInfo,
  type Role,
} from "@/api/members";
import { MemberDialog } from "./member-dialog";

const ASSIGNABLE_ROLES: Role[] = ["editor", "viewer"];

export function FamilyPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const members = useMembers();
  const updateRole = useUpdateMemberRole();
  const removeMember = useRemoveMember();

  const [dialogOpen, setDialogOpen] = useState(false);
  const [removeTarget, setRemoveTarget] = useState<MemberInfo | null>(null);

  const isOwner = session?.role === "owner";

  if (members.isLoading) {
    return <div className="text-muted-foreground">{t("app.loading")}</div>;
  }
  if (members.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("app.error")}</AlertDescription>
      </Alert>
    );
  }

  const list = members.data ?? [];

  const confirmRemove = () => {
    if (!removeTarget) return;
    removeMember.mutate(removeTarget.id, {
      onSuccess: () => setRemoveTarget(null),
    });
  };

  return (
    <div className="grid gap-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">{t("family.title")}</h1>
        {isOwner && <Button onClick={() => setDialogOpen(true)}>{t("family.add")}</Button>}
      </div>

      {updateRole.isError && (
        <Alert variant="destructive">
          <AlertDescription>{t("family.roleChangeError")}</AlertDescription>
        </Alert>
      )}

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("family.columns.name")}</TableHead>
            <TableHead>{t("family.columns.role")}</TableHead>
            {isOwner && <TableHead className="w-10" />}
          </TableRow>
        </TableHeader>
        <TableBody>
          {list.map((member) => {
            const isSelf = member.id === session?.user.id;
            const canEditRole = isOwner && member.role !== "owner";
            const canRemove = isOwner && member.role !== "owner" && !isSelf;
            return (
              <TableRow key={member.id}>
                <TableCell>
                  <div className="font-medium">{member.display_name}</div>
                  <div className="text-xs text-muted-foreground">{member.username}</div>
                </TableCell>
                <TableCell>
                  {canEditRole ? (
                    <Select
                      value={member.role}
                      onValueChange={(v) =>
                        updateRole.mutate({ userId: member.id, body: { role: v as Role } })
                      }
                    >
                      <SelectTrigger className="w-32"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        {ASSIGNABLE_ROLES.map((r) => (
                          <SelectItem key={r} value={r}>{t(`roles.${r}`)}</SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  ) : (
                    <Badge variant="secondary">{t(`roles.${member.role}`)}</Badge>
                  )}
                </TableCell>
                {isOwner && (
                  <TableCell>
                    {canRemove && (
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={t("family.remove")}
                        onClick={() => setRemoveTarget(member)}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    )}
                  </TableCell>
                )}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>

      <MemberDialog open={dialogOpen} onOpenChange={setDialogOpen} />

      <Dialog
        open={removeTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setRemoveTarget(null);
            removeMember.reset();
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("family.remove")}</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            {t("family.removeConfirm", { name: removeTarget?.display_name ?? "" })}
          </p>
          {removeMember.isError && (
            <Alert variant="destructive">
              <AlertDescription>{removeMember.error.message}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setRemoveTarget(null)}>
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={removeMember.isPending}
              onClick={confirmRemove}
            >
              {t("family.remove")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
