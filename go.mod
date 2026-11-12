module github.com/jzbz/gflex

go 1.27

require (
	github.com/spf13/cobra v1.10.2
	golang.org/x/sys v0.50.0
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	// Held on 1.0.x: v1.1.0 renders every bool flag in --help as
	// --flag[=true|false], with no option to turn it off.
	github.com/spf13/pflag v1.0.10 // indirect
)
