// Package tinvest imports a T-Invest (T-Bank) brokerage account. It talks to the
// gateway's unary REST surface with net/http and encoding/json; the official Go
// SDK pulls in go-sqlite3, a cgo package, and the image builds with
// CGO_ENABLED=0.
//
//   - client.go, wire.go: the gateway; nothing else talks to the network.
//   - store.go, sync.go: the mirror, an append-only copy of what the broker
//     said, matched by content because the broker's ids may change.
//   - resolver.go: the broker's instruments against this instance's catalog.
//   - projection.go: mirror rows turned into journal operations, a pure function
//     of one row, so rules can change and history be rebuilt offline.
//   - rebuild.go: the difference between the journal and what projection asks
//     for, computed whole and applied.
//   - reconcile.go: our positions and cash checked against the broker's, and the
//     balance mark that leaves on the account.
//   - settlements.go: each trade's settlement day from the broker report, laid
//     onto trades by the rebuild (Р-3).
package tinvest
