import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useTranslation } from "react-i18next";
import { Pencil, Trash2 } from "lucide-react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { formatMinor, formatPriceIn, signClass } from "@/lib/money";
import { formatDate } from "@/lib/dates";
import { resolveDisplayAmount } from "@/lib/display-amount";
import type { DisplayCurrencyMode } from "@/lib/display-currency";
import { useReportScreenCurrencies } from "@/lib/screen-currencies";
import { MoneyCell } from "@/components/money-cell";
import { costBasisCaveat } from "@/components/cost-basis-notice";
import { unnameableGap } from "@/lib/unnameable-gap";
import {
  useOperations,
  useDeleteOperation,
  useSetOperationCategory,
  useFileByRules,
  editDialogOf,
  OPERATION_TYPES,
  type JournalFilter,
  type OperationType,
  ownedByHand,
  isConflict,
  JOURNAL_PAGE_SIZE,
  type Operation,
  type OperationInBaseGap,
} from "@/api/operations";
import { useInstrumentIndex, type Instrument } from "@/api/instruments";
import { rulePattern, treeOf, useCategories, useCreateCategoryRule, type Category } from "@/api/categories";
import { CategoryChip, categoryKindOf } from "@/components/category-picker";
import type { CostBasisRules } from "@/api/tax-residencies";
import { QueryGate, RefreshFailedNotice } from "@/components/query-notice";
import { queryState, refreshFailed } from "@/lib/query-state";
import type { PricedPaper } from "./purchase-price-dialog";
import { WithheldAbroadNote } from "./withheld-abroad";

// Whether this row's amount is a cost basis assembled from earlier purchases,
// the only figure the cost-basis caveat is true of. Answered by the server's
// assembled_from_lots (on the operation, present with or without in_base, #67),
// set from a stored breakdown rather than the type, so no client list of types
// can drift. has_undated_lots is not used (#81): it also covers a basis typed by
// hand, where no queue chose anything. A legacy transfer whose basis the queue did
// choose loses the caveat too, since nothing on the wire distinguishes it.
function wasAssembledFromLots(operation: Operation): boolean {
  return operation.assembled_from_lots;
}

// The sentence captioning both money cells of a row, from the term the server
// stopped on (Operation.in_base_gap), the row's only source. in_base is whole or
// nothing, so each sentence explains the whole row. The permanent cause (no
// purchase date) is told apart from the temporary ones (a missing rate the
// backfill may bring); those say what happens if a rate appears, as a condition
// (#105), since the wire does not say which pairs ever get one. Wordings match the
// positions screen where the cause is the same. A switch with literal keys for
// scripts/check-i18n.mjs.
function rowGapTitle(
  t: (key: string) => string,
  gap: OperationInBaseGap | null | undefined,
): string {
  switch (gap) {
    case "undated_lot":
      return t("operations.notConvertedUndatedLot");
    case "no_rate_operation_date":
      return t("operations.notConvertedNoRateOperationDate");
    // #79: the amount is a basis valued at each purchase's own day, and one
    // of those days has no rate; the transfer's own date usually has one. The
    // sentence names the rule, since a purchase may fall on the transfer
    // day.
    case "no_rate_lot_date":
      return t("operations.notConvertedNoRateLotDate");
    case null:
    case undefined:
      // A null in_base with a different currency always has a cause; the
      // general phrase claims only what the payload shows.
      return t("operations.notConverted");
    default:
      // A newer server's unknown cause (#105): claim nothing about it, as the
      // positions screen does.
      return unnameableGap(gap, t("operations.notConverted"));
  }
}

// Who owns a row, as the server decides (operation.OwnedByHand): hand-entered
// or loaded from the person's own table is theirs; anything else is a projection
// rebuilt elsewhere and cannot be deleted.
function isImported(operation: Operation): boolean {
  return !ownedByHand(operation);
}

// Who wrote the row: Т-Инвестиции by name, «загружено извне» for any other
// non-manual source, nothing for a hand entry.
function SourceBadge({ operation }: { operation: Operation }) {
  const { t } = useTranslation();
  if (operation.source === "csv") {
    return (
      <Badge variant="outline" title={t("operations.fromTableTitle")}>
        {t("operations.fromTable")}
      </Badge>
    );
  }
  if (!isImported(operation)) return null;
  const tinvest = operation.source === "tinvest";
  return (
    // The hint branches like the label: only T-Invest rows are written again
    // from the broker's mirror; other sources have nothing to rebuild from.
    <Badge
      variant="outline"
      title={tinvest ? t("operations.importedTinvestTitle") : t("operations.importedTitle")}
    >
      {tinvest ? t("connections.tinvest") : t("operations.importedElsewhere")}
    </Badge>
  );
}

// TradingModeBadge says where an operation happened, when known. It shows the
// broker's code and adds a word only where one is sourced. Not a tax statement:
// «обращающаяся» is a property of the security, not of one deal.
function TradingModeBadge({ operation }: { operation: Operation }) {
  const { t } = useTranslation();
  const code = operation.trading_mode;
  const kind = operation.trading_mode_kind;
  if (!code) return null;
  const known = kind && kind !== "unknown";
  return (
    <Badge
      variant="outline"
      data-testid="operation-trading-mode"
      title={t(`tradingModeTitles.${known ? kind : "unknown"}`)}
    >
      {known ? `${t(`tradingModes.${kind}`)} · ${code}` : code}
    </Badge>
  );
}

export function OperationsTable({
  accountId,
  canDelete,
  mode,
  baseCurrency,
  costBasisRules,
  onPurchasePrice,
  onEdit,
  papers = [],
  accountName,
  instrumentLinks = false,
}: {
  accountId: string;
  // Delete action is editor+ (owner/editor); viewers never see it.
  canDelete: boolean;
  mode: DisplayCurrencyMode;
  // The space's base currency, to tell "nothing to convert" from "no rate for
  // that date" when in_base is null (see resolveDisplayAmount).
  baseCurrency: string;
  // Whether the earliest-purchases-first queue is the owner's country's rule
  // (SessionInfo.cost_basis_rules), passed in from the screen. Undefined while the
  // session loads; the caveat waits.
  costBasisRules?: CostBasisRules;
  // What «цена покупки» on an arrival from another broker opens; absent for a
  // reader who cannot write.
  onPurchasePrice?: (paper: PricedPaper) => void;
  // What «изменить» opens: the row's own dialog, filled in (see editDialogOf).
  // Absent for a reader who cannot write.
  onEdit?: (operation: Operation, instrument: Instrument | null) => void;
  // The papers the account has held, for the journal's filter by paper.
  papers?: { id: string; name: string }[];
  // An account's name, for a row that is half of a move between two.
  accountName?: (id: string) => string | undefined;
  // Whether a paper's name leads to its own page. Off where the table is drawn
  // outside the application's router.
  instrumentLinks?: boolean;
}) {
  const { t } = useTranslation();
  // "Show more" appends the next page (see useOperations); the order is
  // stable, so pages neither repeat nor skip rows.
  const [filter, setFilter] = useState<JournalFilter>({});
  const filtered = Object.values(filter).some((v) => v !== undefined && v !== "");
  const operations = useOperations(accountId, JOURNAL_PAGE_SIZE, filter);
  // Instrument names for the whole catalog, not one page: a broker import
  // brings about a hundred papers, and a missing name cannot say it simply was
  // not fetched (#104). See useInstrumentIndex for the cost.
  const instruments = useInstrumentIndex();
  const deleteOperation = useDeleteOperation();
  const [deleteTarget, setDeleteTarget] = useState<Operation | null>(null);
  // The family's categories name the rows' ones; a journal still reads
  // without them.
  const categories = useCategories();
  const categoryList = categories.data ?? [];
  const setCategory = useSetOperationCategory();
  const createRule = useCreateCategoryRule();
  const fileByRules = useFileByRules();
  // A rule remembered from a row files the account's other rows like it at
  // once. It looks in both fields: a bank's statement puts the shop in its
  // description as often as in a counterparty column.
  const remember = (pattern: string, categoryId: string) =>
    createRule.mutate(
      { category_id: categoryId, field: "any", pattern: rulePattern(pattern) },
      { onSuccess: () => fileByRules.mutate(accountId) },
    );
  const list = operations.data?.pages.flatMap((page) => page.operations) ?? [];

  // The journal reports its currencies to the screen-wide counter that decides
  // whether the display-currency toggle shows; it owns its query, and a foreign
  // operation on a base-currency account is otherwise invisible. Only loaded pages
  // count. Before the early returns (Rules of Hooks).
  useReportScreenCurrencies([
    ...list.map((operation) => operation.currency),
    // The conversion target too, so a journal in one foreign currency counts
    // as two.
    ...(baseCurrency ? [baseCurrency] : []),
  ]);

  // A row's amounts are in the operation's own currency, so MoneyCell's default
  // wording would be wrong; converted figures use the rate of the operation's day.
  // Which cause a row without base-currency figures gets comes from the server
  // (Operation.in_base_gap) via rowGapTitle. The positions screen has its own set of
  // causes for its own terms; wordings are shared where the condition is the same.
  // Resolved per row below.

  // The caveat that a cost basis was picked by a queue that is not the owner's
  // country's, only on cells whose figure is one. Undefined while the session loads
  // or when the country's rule is what is computed (see costBasisCaveat).
  const costBasisTitle = costBasisRules ? costBasisCaveat(t, costBasisRules) : undefined;

  // The catalog entry behind a row; undefined for a row without one or while
  // the catalog loads.
  const instrumentOf = (instrumentId: string | null | undefined): Instrument | undefined =>
    instrumentId ? instruments.get(instrumentId) : undefined;

  const instrumentName = (instrumentId: string | null | undefined) => {
    if (!instrumentId) return "—";
    const found = instrumentOf(instrumentId);
    return found ? found.name : `#${instrumentId.slice(-8)}`;
  };

  // What the price column is (#75): money per unit, the only price an operation
  // records (the trade dialog sends the money field). The positions table shows a
  // bond's quote as a percentage of face (#32), and both are on one page. The unit
  // is named in the header and the tooltip, with a sentence added for bonds; the
  // cell also carries its currency (#114, below). The bond sentence adds to the
  // general one, so an instrument not loaded still gets a true tooltip. Two
  // literal-key branches for scripts/check-i18n.mjs.
  const priceTitle = (instrument: Instrument | undefined): string => {
    const title = t("operations.pricePerUnit");
    if (instrument?.type === "bond") {
      return title + "\n" + t("operations.priceNotPercentOfFace");
    }
    return title;
  };

  const confirmDelete = () => {
    if (!deleteTarget) return;
    deleteOperation.mutate(
      { operationId: deleteTarget.id, accountId },
      { onSuccess: () => setDeleteTarget(null) },
    );
  };

  const state = queryState(operations);
  if (state !== "ready") return <QueryGate state={state} />;

  const filters = (
    <JournalFilters
      filter={filter}
      onChange={setFilter}
      papers={papers}
      categories={categoryList}
      active={filtered}
    />
  );
  if (list.length === 0) {
    return (
      <div className="grid gap-3">
        {(filtered || papers.length > 0) && filters}
        <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
          {filtered ? t("operations.emptyFiltered") : t("operations.empty")}
        </div>
      </div>
    );
  }

  // The server's answer, never the page length: a full page cannot tell
  // whether more exists (#86).
  const canLoadMore = operations.hasNextPage;

  return (
    <div className="grid gap-3">
      {filters}
      {/* The rows waiting for a category, and the family's rules to file them. */}
      {canDelete && filter.category === UNFILED && (
        <FileByRulesBar
          pending={fileByRules.isPending}
          filed={fileByRules.isSuccess ? fileByRules.data : undefined}
          failed={fileByRules.isError}
          onFile={() => fileByRules.mutate(accountId)}
        />
      )}
      <RefreshFailedNotice show={refreshFailed(operations)} />
      {createRule.isError && (
        <Alert variant="destructive">
          <AlertDescription>{t("categoryRules.failed")}</AlertDescription>
        </Alert>
      )}
      {setCategory.isError && (
        <Alert variant="destructive" data-testid="operation-category-error">
          <AlertDescription>{t("categoryPicker.failed")}</AlertDescription>
        </Alert>
      )}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("operations.columns.date")}</TableHead>
            <TableHead>{t("operations.columns.type")}</TableHead>
            <TableHead>{t("operations.columns.instrument")}</TableHead>
            <TableHead className="text-right">{t("operations.columns.qty")}</TableHead>
            <TableHead className="text-right">{t("operations.columns.amount")}</TableHead>
            <TableHead className="text-right">{t("operations.columns.fee")}</TableHead>
            {canDelete && <TableHead className="w-10" />}
          </TableRow>
        </TableHeader>
        <TableBody>
          {list.map((operation) => {
            // Amount and fee are converted and rounded separately by the backend.
            const inBase = operation.in_base;
            // convertedTerm ties a term to OperationInBase.currency, never the session's
            // base currency, which a cached journal can outlive (#106), and picks it
            // from the block so a cell cannot be handed the wrong figure.
            const convertedTerm = (
              term: (block: NonNullable<typeof inBase>) => number | null | undefined,
            ) =>
              inBase && {
                amountMinor: term(inBase),
                currency: inBase.currency,
                rateOn: inBase.rate_on,
              };
            const resolvedAmount = resolveDisplayAmount(
              mode,
              operation.currency,
              operation.amount_minor,
              baseCurrency,
              convertedTerm((block) => block.amount_minor),
            );
            const resolvedFee = resolveDisplayAmount(
              mode,
              operation.currency,
              operation.fee_minor,
              baseCurrency,
              convertedTerm((block) => block.fee_minor),
            );
            // Three things a converted amount can be, and the tooltip must name the
            // right one:
            //
            //   - a transfer assembled from lots (assembled_from_lots), checked first:
            //     each piece at its own purchase day's rate, the headline being the
            //     newest purchase. The sentence names the rule, since a purchase may
            //     fall on the transfer day (CheckTransferLots allows it).
            //   - the rate of the row's own day, or
            //   - the nearest earlier rate.
            //
            // dated_on is the day a figure is valued at; rate_on the day its rate came
            // from (weekends and holidays have none). The transfer sentence is about a
            // purchase and names dated_on (#80); the other two name rate_on, chosen by
            // rate_on === dated_on. A date that cannot be rendered gives no tooltip
            // rather than half a sentence.
            const purchaseDate = inBase ? formatDate(inBase.dated_on) : "";
            const convertedTitle = (rateDate: string | null) => {
              // Unreachable: MoneyCell asks only when showing the converted figure.
              if (!inBase) return undefined;
              if (operation.assembled_from_lots) {
                return purchaseDate
                  ? t("operations.convertedAtPurchaseDates", { date: purchaseDate })
                  : undefined;
              }
              if (!rateDate) return undefined;
              return inBase.rate_on === inBase.dated_on
                ? t("operations.convertedAtDate", { date: rateDate })
                : t("operations.convertedAtEarlierDate", { date: rateDate });
            };
            const unconvertedTitle = rowGapTitle(t, operation.in_base_gap);
            return (
              <TableRow key={operation.id}>
                <TableCell className="whitespace-nowrap">{formatDate(operation.occurred_on)}</TableCell>
                <TableCell>
                  <div className="flex flex-wrap items-center gap-1">
                    <Badge variant="secondary">{t(`operationTypes.${operation.type}`)}</Badge>
                    <SourceBadge operation={operation} />
                    <TradingModeBadge operation={operation} />
                  </div>
                  {operation.counterpart_account_id && (
                    <div className="text-xs text-muted-foreground" data-testid="operation-counterpart">
                      {operation.type === "withdrawal" || operation.type === "transfer_out"
                        ? t("operations.toAccount", {
                            name: accountName?.(operation.counterpart_account_id) ?? t("operations.otherAccount"),
                          })
                        : t("operations.fromAccount", {
                            name: accountName?.(operation.counterpart_account_id) ?? t("operations.otherAccount"),
                          })}
                    </div>
                  )}
                  {/* A basis typed by hand: the family's total moved by the difference
                     the server worked out. Said on both halves. */}
                  {operation.stated_basis_change_minor != null && (
                    <div className="max-w-64 text-xs whitespace-normal text-amber-700 dark:text-amber-400" data-testid="operation-stated-basis">
                      {operation.stated_basis_change_minor === 0
                        ? t("operations.statedBasisSame")
                        : t("operations.statedBasisChange", {
                            change: `${operation.stated_basis_change_minor > 0 ? "+" : ""}${formatMinor(
                              operation.stated_basis_change_minor,
                              operation.currency,
                            )}`,
                          })}
                    </div>
                  )}
                  {operation.withheld_abroad && <WithheldAbroadNote operation={operation} editable={canDelete} />}
                  {/* What the money was for and with whom; anyone who may write
                     files the row from here, a broker's row included. */}
                  {operation.categorizable && (
                    <div className="mt-1">
                      <CategoryChip
                        categories={categoryList}
                        kind={categoryKindOf(operation.type)}
                        value={operation.category_id ?? null}
                        editable={canDelete}
                        pending={setCategory.isPending && setCategory.variables?.operationId === operation.id}
                        onChange={(categoryId) => setCategory.mutate({ operationId: operation.id, categoryId })}
                        counterparty={operation.counterparty || operation.note}
                        onRemember={(categoryId) => remember(operation.counterparty || operation.note, categoryId)}
                      />
                    </div>
                  )}
                  {operation.counterparty && (
                    <div className="text-xs text-muted-foreground" data-testid="operation-counterparty">
                      {operation.counterparty}
                    </div>
                  )}
                </TableCell>
                <TableCell>
                  {instrumentLinks && operation.instrument_id ? (
                    <Link
                      to="/instruments/$instrumentId"
                      params={{ instrumentId: operation.instrument_id }}
                      className="hover:underline"
                    >
                      {instrumentName(operation.instrument_id)}
                    </Link>
                  ) : (
                    instrumentName(operation.instrument_id)
                  )}
                  {onPurchasePrice &&
                    operation.type === "transfer_in" &&
                    operation.transfer_group_id == null &&
                    operation.instrument_id && (
                      <button
                        type="button"
                        data-testid="operation-purchase-price"
                        className="ml-2 text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground"
                        onClick={() => {
                          const found = instrumentOf(operation.instrument_id);
                          onPurchasePrice({
                            id: operation.instrument_id as string,
                            name: found?.name ?? instrumentName(operation.instrument_id),
                            ticker: found?.ticker ?? "",
                          });
                        }}
                      >
                        {t("operations.purchasePrice")}
                      </button>
                    )}
                  {/* The broker's (or person's) own note, under the instrument: the badge
                     says the category, the note the event («Погашение Инарктика
                     001Р-01»). Shown as written. */}
                  {operation.note && (
                    <div
                      data-testid="operation-note"
                      className="text-xs text-muted-foreground"
                    >
                      {operation.note}
                    </div>
                  )}
                </TableCell>
                <TableCell
                  className="text-right tabular-nums"
                  // On the whole cell: "100 ×" is most of its width.
                  title={
                    operation.quantity && operation.price
                      ? priceTitle(instrumentOf(operation.instrument_id))
                      : undefined
                  }
                >
                  {operation.quantity && operation.price ? (
                    <>
                      {operation.quantity} ×{" "}
                      <span data-testid="operation-price">
                        {/* The price carries the operation's own currency (#114), read off
                           the operation, never off the row's other figures, which convert in
                           base mode while the price never does. On every row, so it does not
                           change with the toggle (as the positions screen's quote, #76).
                           formatPriceIn shares formatPrice's parse, keeping sub-cent prices
                           readable (#30). Unparseable input is shown raw, without a
                           currency. */}
                        {formatPriceIn(operation.price, operation.currency) ?? operation.price}
                      </span>
                    </>
                  ) : (
                    "—"
                  )}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <MoneyCell
                    resolved={resolvedAmount}
                    className={signClass(resolvedAmount.amountMinor)}
                    notConvertedTitle={unconvertedTitle}
                    convertedTitle={convertedTitle}
                    // Only on an amount that is a cost basis; the fee is a charge on its own
                    // day.
                    caveatTitle={wasAssembledFromLots(operation) ? costBasisTitle : undefined}
                    testId="operation-amount"
                  />
                </TableCell>
                <TableCell className="text-right tabular-nums text-muted-foreground">
                  {/* A zero fee is nothing in any currency; the dash stays. */}
                  {operation.fee_minor > 0 ? (
                    <MoneyCell
                      resolved={resolvedFee}
                      notConvertedTitle={unconvertedTitle}
                      convertedTitle={convertedTitle}
                      testId="operation-fee"
                    />
                  ) : (
                    "—"
                  )}
                </TableCell>
                {/* The cell stays so imported rows keep their columns; only the action
                   goes (see isImported). */}
                {canDelete && (
                  <TableCell>
                    <div className="flex">
                      {onEdit && editDialogOf(operation) ? (
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={t("operations.edit")}
                          onClick={() =>
                            onEdit(operation, instrumentOf(operation.instrument_id) ?? null)
                          }
                        >
                          <Pencil className="size-4" />
                        </Button>
                      ) : (
                        // Keeps the bin in its column on rows without an edit action.
                        onEdit && <span aria-hidden="true" className="size-8 shrink-0" />
                      )}
                      {!isImported(operation) && (
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={t("operations.delete")}
                          onClick={() => setDeleteTarget(operation)}
                        >
                          <Trash2 className="size-4" />
                        </Button>
                      )}
                    </div>
                  </TableCell>
                )}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>

      {canLoadMore && (
        <Button
          variant="outline"
          disabled={operations.isFetchingNextPage}
          onClick={() => void operations.fetchNextPage()}
        >
          {operations.isFetchingNextPage ? t("app.loading") : t("operations.loadMore")}
        </Button>
      )}

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(null);
            deleteOperation.reset();
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("operations.delete")}</DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            {t("operations.deleteConfirm", {
              date: deleteTarget ? formatDate(deleteTarget.occurred_on) : "",
            })}
          </p>
          {deleteOperation.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(deleteOperation.error)
                  ? t("operations.deleteConflict")
                  : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                // Reset here too: Radix calls onOpenChange only for its own dismiss
                // triggers, so a failed attempt's error would leak into the next
                // dialog.
                setDeleteTarget(null);
                deleteOperation.reset();
              }}
            >
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={deleteOperation.isPending}
              onClick={confirmDelete}
            >
              {t("operations.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

const ALL = "all";
const UNFILED = "none";

// FileByRulesBar offers to file the waiting rows by the family's rules and
// says what came of it.
export function FileByRulesBar({
  pending,
  filed,
  failed,
  onFile,
}: {
  pending: boolean;
  filed?: number;
  failed: boolean;
  onFile: () => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-wrap items-center gap-3 text-sm" data-testid="file-by-rules">
      <Button size="sm" variant="outline" disabled={pending} onClick={onFile}>
        {t("fileByRules.button")}
      </Button>
      {filed !== undefined && (
        <span className="text-muted-foreground">
          {filed > 0 ? t("fileByRules.filed", { rows: filed }) : t("fileByRules.none")}
        </span>
      )}
      {failed && <span className="text-destructive">{t("fileByRules.failed")}</span>}
    </div>
  );
}

// The journal's filter: a type, a paper the account has held, a category, a
// period.
function JournalFilters({
  filter,
  onChange,
  papers,
  categories,
  active,
}: {
  filter: JournalFilter;
  onChange: (filter: JournalFilter) => void;
  papers: { id: string; name: string }[];
  categories: Category[];
  active: boolean;
}) {
  const { t } = useTranslation();
  // Spending first, then earning, each child under its parent; archived ones
  // too, since rows keep them.
  // «Подарки» may be spent and received, so each kind is a group of its own.
  const categoryGroups = (["expense", "income"] as const)
    .map((kind) => ({
      kind,
      options: treeOf(categories, kind).flatMap((top) => [
        { category: top as Category, child: false },
        ...top.children.map((child) => ({ category: child, child: true })),
      ]),
    }))
    .filter((group) => group.options.length > 0);
  return (
    <div className="flex flex-wrap items-end gap-2 text-sm" data-testid="journal-filters">
      <div className="grid gap-1">
        <Label className="text-xs text-muted-foreground">{t("operations.filter.type")}</Label>
        <Select
          value={filter.type ?? ALL}
          onValueChange={(v) => onChange({ ...filter, type: v === ALL ? undefined : (v as OperationType) })}
        >
          <SelectTrigger className="h-8 w-44" aria-label={t("operations.filter.type")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL}>{t("operations.filter.all")}</SelectItem>
            {OPERATION_TYPES.map((type) => (
              <SelectItem key={type} value={type}>
                {t(`operationTypes.${type}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {papers.length > 0 && (
        <div className="grid gap-1">
          <Label className="text-xs text-muted-foreground">{t("operations.filter.paper")}</Label>
          <Select
            value={filter.instrumentId ?? ALL}
            onValueChange={(v) => onChange({ ...filter, instrumentId: v === ALL ? undefined : v })}
          >
            <SelectTrigger className="h-8 w-52" aria-label={t("operations.filter.paper")}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>{t("operations.filter.allPapers")}</SelectItem>
              {papers.map((paper) => (
                <SelectItem key={paper.id} value={paper.id}>
                  {paper.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
      {categoryGroups.length > 0 && (
        <div className="grid gap-1">
          <Label className="text-xs text-muted-foreground">{t("operations.filter.category")}</Label>
          <Select
            value={filter.category ?? ALL}
            onValueChange={(v) => onChange({ ...filter, category: v === ALL ? undefined : v })}
          >
            <SelectTrigger className="h-8 w-52" aria-label={t("operations.filter.category")}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent className="max-h-80">
              <SelectItem value={ALL}>{t("operations.filter.allCategories")}</SelectItem>
              <SelectItem value={UNFILED}>{t("operations.filter.unfiled")}</SelectItem>
              {categoryGroups.map((group) => (
                <SelectGroup key={group.kind}>
                  <SelectLabel>{t(`categories.kinds.${group.kind}`)}</SelectLabel>
                  {group.options.map(({ category, child }) => (
                    <SelectItem key={category.id} value={category.id} className={child ? "pl-6" : undefined}>
                      {category.archived
                        ? t("operations.filter.archivedCategory", { name: category.name })
                        : category.name}
                    </SelectItem>
                  ))}
                </SelectGroup>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
      <div className="grid gap-1">
        <Label htmlFor="journal-from" className="text-xs text-muted-foreground">
          {t("operations.filter.from")}
        </Label>
        <Input
          id="journal-from"
          type="date"
          className="h-8 w-36"
          value={filter.from ?? ""}
          onChange={(e) => onChange({ ...filter, from: e.target.value || undefined })}
        />
      </div>
      <div className="grid gap-1">
        <Label htmlFor="journal-to" className="text-xs text-muted-foreground">
          {t("operations.filter.to")}
        </Label>
        <Input
          id="journal-to"
          type="date"
          className="h-8 w-36"
          value={filter.to ?? ""}
          onChange={(e) => onChange({ ...filter, to: e.target.value || undefined })}
        />
      </div>
      {active && (
        <Button variant="ghost" size="sm" className="h-8" onClick={() => onChange({})}>
          {t("operations.filter.reset")}
        </Button>
      )}
    </div>
  );
}
