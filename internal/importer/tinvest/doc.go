// Package tinvest is a thin REST client for T-Bank's T-Invest API. It talks
// only to the gateway's unary REST surface (POST + JSON) using the standard
// library's net/http and encoding/json — no gRPC, no streams, and no
// third-party HTTP dependency.
//
// This is a hand-written client rather than the official Go SDK
// (github.com/russianinvestments/invest-api-go-sdk) because that SDK's own
// README documents a transitive dependency on go-sqlite3, a cgo package
// (github.com/mattn/go-sqlite3#installation) — and this project's container
// image is built with CGO_ENABLED=0 (see Dockerfile), which the SDK's build
// would not survive.
//
// The package has since grown past the client, and the sentence that used to
// stand here — that storage, synchronization and turning broker operations
// into this application's journal were all out of scope — stopped being true
// as those parts arrived. What is here now, and what each part may touch:
//
//   - client.go, wire.go: the gateway. Nothing else in this package talks to
//     the network.
//   - store.go, sync.go: the mirror — an append-only local copy of what the
//     broker said, matched by content rather than by the broker's own
//     identifiers, which its documentation says may change.
//   - resolver.go: the broker's instruments against this instance's catalog.
//   - projection.go: the mirror turned into journal operations, as a pure
//     function of one mirror row — no database, no clock — so the rules can
//     change and the whole history be rebuilt without going near the API.
package tinvest
