module github.com/deepnoodle-ai/dashi/cmd/dashi

go 1.26.0

toolchain go1.26.8

require github.com/deepnoodle-ai/dashi v0.0.0

require (
	github.com/deepnoodle-ai/wonton v0.2.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/deepnoodle-ai/dashi => ../..
