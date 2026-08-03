package db

// Migrations is the embedded migration directory, exported for the migration
// tests alone (this file is compiled into the test binary only). A test that
// has to stand a database at one particular version — in order to put in it
// the very rows a later migration must refuse — drives goose itself, and goose
// needs the same embedded files Migrate hands it.
var Migrations = migrations
