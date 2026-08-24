-- +goose Up
-- A fourth reason a sync can run: the corporate-actions registry changed a
-- journal this connection is reconciled against.
--
-- WHY IT IS A REASON OF ITS OWN AND NOT ONE OF THE THREE. The run log's trigger
-- is what a reader is told about why a check happened, and the three that
-- existed each name something that is not true of this one: `schedule` is the
-- hourly job, `manual` is the owner pressing the button, `initial` is the first
-- import. A registry-caused run is none of them — nobody asked for it and the
-- clock did not strike — and filing it under any of the three would put a
-- sentence on the screen that the run itself contradicts. This program has been
-- caught four times by a true number under a false caption; a fourth word is
-- cheaper than a fifth occasion.
--
-- WHAT MAKES IT NECESSARY. The registry writes a split into an account's journal
-- long after that account was last reconciled — on the live stand the gap was
-- three seconds, because both run at startup and the reconciliation happened to
-- go first. The verdict then goes on naming a difference the journal no longer
-- has until the next hourly run: FXUS 3 771 against the broker's 8 830, when the
-- journal had just been corrected to 8 820. The figure was right when it was
-- struck and wrong by the time it was read, which is the one thing this program
-- refuses to leave alone.
ALTER TABLE tinvest_sync_runs DROP CONSTRAINT tinvest_sync_runs_trigger_check;
ALTER TABLE tinvest_sync_runs ADD CONSTRAINT tinvest_sync_runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'initial', 'registry'));

-- +goose Down
-- Rows recorded under the new trigger are rewritten to 'schedule' rather than
-- deleted: they are real runs whose results a reader may still be looking at,
-- and 'schedule' is the closest of the three that remain — an automatic run
-- nobody asked for. Down migrations here are for a rollback of the code, and a
-- rollback must not silently drop a check that happened.
UPDATE tinvest_sync_runs SET trigger = 'schedule' WHERE trigger = 'registry';
ALTER TABLE tinvest_sync_runs DROP CONSTRAINT tinvest_sync_runs_trigger_check;
ALTER TABLE tinvest_sync_runs ADD CONSTRAINT tinvest_sync_runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'initial'));
