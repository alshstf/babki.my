package operation

// SortJournalForTest exposes the in-memory half of the fold order to the
// package's external tests.
//
// It exists so that ONE test can compare the two spellings of that order — the
// SQL the database sorts by and the comparison the write paths use — against
// each other on the same rows. Both are unexported, and rightly: nothing
// outside this package assembles a journal to fold. But a rule written twice
// needs a test that reads both, and that test lives with the rest of the
// journal's tests in operation_test.
//
// The engine order the database applies is not exposed here because it does not
// need to be: it is observable through any ordinary read (ListForEngine), which
// is exactly how a caller meets it.
func SortJournalForTest(journal []Operation) { sortJournal(journal) }
