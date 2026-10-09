import { useTranslation } from "react-i18next";
import { LoaderCircle } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Progress } from "@/components/ui/progress";
import { useAccounts } from "@/api/accounts";
import {
  overallShare,
  taskShare,
  tasksOfAccount,
  useBackgroundTasks,
  useRefreshWhenTasksEnd,
  type BackgroundTask,
} from "@/api/background";

// The i18n key of a job kind: dots would be read as nesting.
const kindKey = (kind: string) => kind.replaceAll(".", "_");

const percentOf = (share: number | null) => (share === null ? null : Math.round(share * 100));

// TaskTitle is a task's name; TaskStage what it is doing now and how far, the
// count only when the stage has one.
function useTaskText() {
  const { t } = useTranslation();
  return {
    title: (task: BackgroundTask) => t(`background.kinds.${kindKey(task.kind)}`, { defaultValue: t("background.kinds.other") }),
    stage: (task: BackgroundTask) => {
      const stage = task.stage ? t(`background.stages.${task.stage}`, { defaultValue: "" }) : "";
      const count = task.total > 0 ? t("background.count", { done: task.done, total: task.total }) : "";
      return [stage, count].filter(Boolean).join(" — ");
    },
  };
}

// BackgroundActivity is the header's indicator of the jobs running now: the
// count and their overall share, and on a click each one with its own bar. It
// is also what reads the screens again when a job ends, so it stays mounted
// while nothing runs and renders nothing then.
export function BackgroundActivity() {
  const { t, i18n } = useTranslation();
  const tasks = useBackgroundTasks();
  useRefreshWhenTasksEnd(tasks.data);
  const accounts = useAccounts();
  const text = useTaskText();
  const list = tasks.data ?? [];
  if (list.length === 0) return null;

  const percent = percentOf(overallShare(list));
  const names = new Map((accounts.data ?? []).map((account) => [account.id, account.name]));
  const time = new Intl.DateTimeFormat(i18n.language, { hour: "2-digit", minute: "2-digit" });
  const label = t("background.indicator", { count: list.length });

  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="sm" aria-label={label} title={label} data-testid="background-indicator">
          <LoaderCircle className="size-4 animate-spin" aria-hidden />
          <span>{list.length}</span>
          {percent !== null && <span className="text-xs text-muted-foreground">{percent}&nbsp;%</span>}
        </Button>
      </PopoverTrigger>
      <PopoverContent data-testid="background-panel">
        <div className="mb-3 text-sm font-medium">{t("background.title")}</div>
        <ul className="grid gap-4">
          {list.map((task) => {
            const share = percentOf(taskShare(task));
            const accountNames = task.account_ids.map((id) => names.get(id)).filter(Boolean);
            return (
              <li key={task.id} className="grid gap-1.5" data-testid="background-task">
                <div className="flex items-baseline justify-between gap-2 text-sm">
                  <span className="font-medium">{text.title(task)}</span>
                  {share !== null && <span className="text-xs text-muted-foreground tabular-nums">{share}&nbsp;%</span>}
                </div>
                <Progress value={share} aria-label={text.title(task)} />
                {text.stage(task) && <div className="text-xs text-muted-foreground">{text.stage(task)}</div>}
                <div className="text-xs text-muted-foreground">
                  {task.whole_instance
                    ? t("background.wholeInstance")
                    : accountNames.length > 0 && t("background.accounts", { names: accountNames.join(", ") })}
                  {" · "}
                  {t("background.since", { time: time.format(new Date(task.started_at)) })}
                </div>
              </li>
            );
          })}
        </ul>
      </PopoverContent>
    </Popover>
  );
}

// AccountBusyNotice says, on an account's page, that a running job is still
// changing its figures; nothing when none is.
export function AccountBusyNotice({ accountId }: { accountId: string }) {
  const { t } = useTranslation();
  const tasks = useBackgroundTasks();
  const text = useTaskText();
  const mine = tasksOfAccount(tasks.data, accountId);
  if (mine.length === 0) return null;
  const task = mine[0];
  const percent = percentOf(taskShare(task));
  return (
    <Alert data-testid="account-busy-notice">
      <LoaderCircle className="size-4 animate-spin" aria-hidden />
      <AlertDescription className="grid gap-2">
        <span>
          {t("background.accountNotice", {
            task: text.title(task),
            stage: text.stage(task) || t("background.starting"),
          })}
        </span>
        <Progress value={percent} aria-label={text.title(task)} className="max-w-sm" />
      </AlertDescription>
    </Alert>
  );
}

// accountsBusy is the set of accounts a running job is still changing, for the
// accounts table.
export function useBusyAccounts(): ReadonlySet<string> {
  const tasks = useBackgroundTasks();
  return new Set((tasks.data ?? []).flatMap((task) => task.account_ids));
}
