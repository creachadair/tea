# tea

[![GoDoc](https://img.shields.io/static/v1?label=godoc&message=reference&color=blue)](https://pkg.go.dev/github.com/creachadair/tea)

This repository contains a tool that triggers actions based on patterns in its
input. It is analogous to a combination of `tee` and `grep`, with the addition
that it can execute an external program in response to matches on the input.

This can be useful for watching log output from a long-running program for
certain interesting patterns, e.g.,

```shell
bundle exec jekyll serve |
   tea \
    -- 'Regenerating:' say "Rebuilding your site" \
    -- '\bdone in (\d+\.\d)\d* seconds' say 'Build complete after $1 seconds'
```

## Installation

```shell
go install github.com/creachadair/tea@latest
```
