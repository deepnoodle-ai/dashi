module github.com/deepnoodle-ai/dashi/cmd/dashi

go 1.26.0

toolchain go1.26.8

require (
	github.com/deepnoodle-ai/dashi v0.0.0
	github.com/deepnoodle-ai/wonton v0.2.2-0.20260930081332-16c0f52d0d68
	golang.org/x/term v0.45.0
)

require (
	github.com/alecthomas/chroma/v2 v2.27.0 // indirect
	github.com/dlclark/regexp2/v2 v2.7.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/yuin/goldmark v1.8.5 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/deepnoodle-ai/dashi => ../..
