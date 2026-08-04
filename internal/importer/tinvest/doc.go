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
// Storage, synchronization, and turning broker operations into this
// application's journal are all out of scope here: this package only knows
// how to ask the gateway questions and hand back typed answers.
package tinvest
