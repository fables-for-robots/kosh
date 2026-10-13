// Package e2e holds kosh's end-to-end tests: kosh's image in a kind
// cluster, installed with its chart, and reached with the real ssh and
// the real iroh-ssh, as its users meet it.
//
// The tests are built only with the build tag e2e, and need what
// scripts/test-e2e.sh sets up and hands them in the environment:
//
//	scripts/test-e2e.sh
//
// Without the tag, only the test of the screen model they read kosh's
// screen with runs: go test ./e2e/.
package e2e
