# shortmux
[![Go Reference](https://pkg.go.dev/badge/github.com/henvic/shortmux.svg)](https://pkg.go.dev/github.com/henvic/shortmux) [![Build Status](https://github.com/henvic/shortmux/workflows/Tests/badge.svg)](https://github.com/henvic/shortmux/actions?query=workflow%3ATests) [![Coverage Status](https://coveralls.io/repos/henvic/shortmux/badge.svg)](https://coveralls.io/r/henvic/shortmux) [![Go Report Card](https://goreportcard.com/badge/github.com/henvic/shortmux)](https://goreportcard.com/report/github.com/henvic/shortmux)

Fork of http.ServeMux with the goal of allowing more flexibility in writing routes (for good and evil). The API is unstable.

**Motivation:** http.NewServeMux has a strict pattern matching logic and panics with a pattern conflict message whenever you try to handle conflicting patterns ambigously.
While this panic can help you, as a developer, write more robust REST API routes, it's both tricky and time-consuming to get around it when using the standard library.

```go
mux := shortmux.NewServeMux() // With http.NewServeMux() this would fail.
mux.HandleFunc("/a/{b}", handler1)
mux.HandleFunc("/{c}/d", handler2) // works fine - most specific wins (reading from left-to-right)
```

**Ideas for next features:**

* Support for setting custom NotFound and MethodNotAllowed HTTP handlers (likely).
* Allow path parameters anywhere on the path instead of limited to segments (unlikely).
* Restore old pattern-matching behavor for existing functions and introduce `HandleOverlapping` to opt-in the current precedence logic per each pattern instead of for everything (likely). This effectively would make this library a drop-in replacement for http.ServeMux.
* Remove Host routing support (unlikely).
