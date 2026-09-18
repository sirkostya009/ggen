module github.com/sirkostya009/ggen/cli

go 1.27

require github.com/sirkostya009/ggen/gen v0.0.0

require (
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/tools v0.46.0 // indirect
)

replace github.com/sirkostya009/ggen/gen => ../gen
