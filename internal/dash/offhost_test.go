package dash

// Placeholder for Task 2's DOM-walk scanner. This blank import alone is what
// keeps golang.org/x/net in go.mod as a real dependency once `go mod tidy`
// runs -- Go's module graph does not distinguish "test-only" from "used",
// but it does require a real importer to keep a dependency past `go mod
// tidy`, and this plan's Task 1 (the dependency add) runs before Task 2
// (this file's real content: scanRenderedHTML, its adversarial self-test,
// and the four-view assertion) is written. Task 2 replaces this entire file
// -- matching 05-03's identical placeholder-then-replace pattern for
// server.go's routes().
import _ "golang.org/x/net/html"
