package operation

// SortJournalForTest exposes the in-memory fold order so one test can compare
// it with the SQL order (ListForEngine) on the same rows.
func SortJournalForTest(journal []Operation) { sortJournal(journal) }
