import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useCheckMailbox, useMailbox, useSetMailbox, type Mailbox } from "@/api/receipts";
import { formatDateTime } from "@/lib/dates";

// MailboxSection is the box read for receipts (decision Р-35): a box kept for
// receipts alone, the family's mail forwarding the shops' and OFD letters
// there, read every hour by its app password, which is never shown again.
export function MailboxSection() {
  const { t } = useTranslation();
  const box = useMailbox();
  const [editing, setEditing] = useState(false);
  const check = useCheckMailbox();
  const b = box.data;
  if (box.isLoading) return null;
  if (!b || editing) {
    return <MailboxForm box={b ?? null} onDone={() => setEditing(false)} />;
  }
  return (
    <div className="grid gap-1 border-t pt-3" data-testid="mailbox">
      <p className="text-sm font-medium">{t("mailbox.title")}</p>
      <p className="text-sm">
        {b.username} · {b.host}
        {b.folder !== "INBOX" && ` · ${b.folder}`}
      </p>
      <p className={b.problem ? "text-xs text-red-700 dark:text-red-400" : "text-xs text-muted-foreground"} data-testid="mailbox-status">
        {b.problem
          ? t(`mailbox.problem.${b.problem}`)
          : b.checked_at
            ? t("mailbox.checked", { when: formatDateTime(b.checked_at), n: b.last_found })
            : t("mailbox.notYet")}
      </p>
      {check.isSuccess && (
        <p className="text-xs" data-testid="mailbox-result">
          {t("mailbox.found", { n: check.data.found })}
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" variant="outline" disabled={check.isPending} onClick={() => check.mutate()}>
          {check.isPending ? t("mailbox.checking") : t("mailbox.check")}
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={() => setEditing(true)}>
          {t("mailbox.edit")}
        </Button>
      </div>
    </div>
  );
}

function MailboxForm({ box, onDone }: { box: Mailbox | null; onDone: () => void }) {
  const { t } = useTranslation();
  const save = useSetMailbox();
  const [host, setHost] = useState(box?.host ?? "imap.yandex.ru");
  const [port, setPort] = useState(String(box?.port ?? 993));
  const [username, setUsername] = useState(box?.username ?? "");
  const [password, setPassword] = useState("");
  const [folder, setFolder] = useState(box?.folder ?? "INBOX");
  const valid = host.trim() !== "" && /^\d{1,5}$/.test(port) && username.trim() !== "" && (box !== null || password !== "");
  const submit = () =>
    save.mutate(
      { host: host.trim(), port: Number(port), username: username.trim(), folder: folder.trim() || "INBOX", password: password === "" ? null : password },
      { onSuccess: onDone },
    );
  const field = (id: string, label: string, value: string, set: (v: string) => void, type = "text", placeholder?: string) => (
    <div className="grid gap-1">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} type={type} value={value} placeholder={placeholder} autoComplete="off" onChange={(e) => set(e.target.value)} />
    </div>
  );
  return (
    <div className="grid gap-2 border-t pt-3" data-testid="mailbox-form">
      <p className="text-sm font-medium">{t("mailbox.title")}</p>
      <p className="text-xs text-muted-foreground">{t("mailbox.how")}</p>
      <div className="grid gap-2 sm:grid-cols-[1fr_6rem]">
        {field("mailbox-host", t("mailbox.host"), host, setHost)}
        {field("mailbox-port", t("mailbox.port"), port, setPort)}
      </div>
      {field("mailbox-username", t("mailbox.username"), username, setUsername)}
      {field("mailbox-password", t("mailbox.password"), password, setPassword, "password", box ? t("mailbox.passwordKept") : undefined)}
      {field("mailbox-folder", t("mailbox.folder"), folder, setFolder)}
      {save.isError && (
        <Alert variant="destructive">
          <AlertDescription>{t("mailbox.failed")}</AlertDescription>
        </Alert>
      )}
      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" disabled={!valid || save.isPending} onClick={submit}>
          {t("common.save")}
        </Button>
        {box && (
          <>
            <Button type="button" size="sm" variant="outline" onClick={onDone}>
              {t("common.cancel")}
            </Button>
            <Button type="button" size="sm" variant="ghost" className="ml-auto" disabled={save.isPending} onClick={() => save.mutate(null, { onSuccess: onDone })}>
              {t("mailbox.forget")}
            </Button>
          </>
        )}
      </div>
    </div>
  );
}
