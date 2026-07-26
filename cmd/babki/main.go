// babki — unified binary for babki.my. Roles: all | api | worker | migrate.
package main

import "os"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
